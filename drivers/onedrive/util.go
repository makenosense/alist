package onedrive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	stdpath "path"
	"strconv"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/operations"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/go-resty/resty/v2"
	jsoniter "github.com/json-iterator/go"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type Host struct {
	Oauth string
	Api   string
}

var onedriveHostMap = map[string]Host{
	"global": {
		Oauth: "https://login.microsoftonline.com",
		Api:   "https://graph.microsoft.com",
	},
	"cn": {
		Oauth: "https://login.chinacloudapi.cn",
		Api:   "https://microsoftgraph.chinacloudapi.cn",
	},
	"us": {
		Oauth: "https://login.microsoftonline.us",
		Api:   "https://graph.microsoft.us",
	},
	"de": {
		Oauth: "https://login.microsoftonline.de",
		Api:   "https://graph.microsoft.de",
	},
}

func (d *Onedrive) GetMetaUrl(auth bool, path string) string {
	host, _ := onedriveHostMap[d.Region]
	if auth {
		return host.Oauth
	}
	if d.IsSharepoint {
		if path == "/" || path == "\\" {
			return fmt.Sprintf("%s/v1.0/sites/%s/drive/root", host.Api, d.SiteId)
		} else {
			return fmt.Sprintf("%s/v1.0/sites/%s/drive/root:%s:", host.Api, d.SiteId, path)
		}
	} else {
		if path == "/" || path == "\\" {
			return fmt.Sprintf("%s/v1.0/me/drive/root", host.Api)
		} else {
			return fmt.Sprintf("%s/v1.0/me/drive/root:%s:", host.Api, path)
		}
	}
}

func (d *Onedrive) refreshToken() error {
	var err error
	for i := 0; i < 3; i++ {
		err = d._refreshToken()
		if err == nil {
			break
		}
	}
	return err
}

type TokenErr struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (d *Onedrive) _refreshToken() error {
	url := d.GetMetaUrl(true, "") + "/common/oauth2/v2.0/token"
	var resp base.TokenResp
	var e TokenErr
	_, err := base.RestyClient.R().SetResult(&resp).SetError(&e).SetFormData(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     d.ClientId,
		"client_secret": d.ClientSecret,
		"redirect_uri":  d.RedirectUri,
		"refresh_token": d.RefreshToken,
	}).Post(url)
	if err != nil {
		return err
	}
	if e.Error != "" {
		return fmt.Errorf("%s", e.ErrorDescription)
	}
	if resp.RefreshToken == "" {
		return errs.EmptyToken
	}
	d.RefreshToken, d.AccessToken = resp.RefreshToken, resp.AccessToken
	operations.MustSaveDriverStorage(d)
	return nil
}

type RespErr struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (d *Onedrive) Request(url string, method string, callback func(*resty.Request), resp interface{}) ([]byte, error) {
	req := base.RestyClient.R()
	req.SetHeader("Authorization", "Bearer "+d.AccessToken)
	if callback != nil {
		callback(req)
	}
	if resp != nil {
		req.SetResult(resp)
	}
	var e RespErr
	req.SetError(&e)
	res, err := req.Execute(method, url)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if e.Error.Code != "" {
		if e.Error.Code == "InvalidAuthenticationToken" {
			err = d.refreshToken()
			if err != nil {
				return nil, err
			}
			return d.Request(url, method, callback, resp)
		}
		return nil, errors.New(e.Error.Message)
	}
	return res.Body(), nil
}

func (d *Onedrive) GetFiles(path string) ([]File, error) {
	var res []File
	nextLink := d.GetMetaUrl(false, path) + "/children?$expand=thumbnails"
	for nextLink != "" {
		var files Files
		_, err := d.Request(nextLink, http.MethodGet, nil, &files)
		if err != nil {
			return nil, err
		}
		res = append(res, files.Value...)
		nextLink = files.NextLink
	}
	return res, nil
}

func (d *Onedrive) GetFile(path string) (*File, error) {
	var file File
	u := d.GetMetaUrl(false, path)
	_, err := d.Request(u, http.MethodGet, nil, &file)
	return &file, err
}

func (d *Onedrive) upSmall(dstDir model.Obj, stream model.FileStreamer) error {
	url := d.GetMetaUrl(false, stdpath.Join(dstDir.GetID(), stream.GetName())) + "/content"
	data, err := io.ReadAll(stream)
	if err != nil {
		return err
	}
	_, err = d.Request(url, http.MethodPut, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *Onedrive) upBig(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	url := d.GetMetaUrl(false, stdpath.Join(dstDir.GetID(), stream.GetName())) + "/createUploadSession"
	res, err := d.Request(url, http.MethodPost, nil, nil)
	if err != nil {
		return err
	}
	uploadUrl := jsoniter.Get(res, "uploadUrl").ToString()
	var finish int64 = 0
	const DEFAULT = 4 * 1024 * 1024
	for finish < stream.GetSize() {
		if utils.IsCanceled(ctx) {
			return ctx.Err()
		}
		log.Debugf("upload: %d", finish)
		var byteSize int64 = DEFAULT
		left := stream.GetSize() - finish
		if left < DEFAULT {
			byteSize = left
		}
		byteData := make([]byte, byteSize)
		n, err := io.ReadFull(stream, byteData)
		log.Debug(err, n)
		if err != nil {
			return err
		}
		req, err := http.NewRequest("PUT", uploadUrl, bytes.NewBuffer(byteData))
		req.Header.Set("Content-Length", strconv.Itoa(int(byteSize)))
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", finish, finish+byteSize-1, stream.GetSize()))
		finish += byteSize
		res, err := base.HttpClient.Do(req)
		if res.StatusCode != 201 && res.StatusCode != 202 {
			data, _ := io.ReadAll(res.Body)
			res.Body.Close()
			return errors.New(string(data))
		}
		res.Body.Close()
		up(int(finish / stream.GetSize()))
	}
	return nil
}
