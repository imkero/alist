package aliyundrive_open

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

// do others that not defined in Driver interface

func (d *AliyundriveOpen) _refreshToken() (string, string, error) {
	var url string
	if strings.HasPrefix(d.OauthTokenURL, "https://api.oplist.org/") {
		url = d.OauthTokenURL
		var resp struct {
			RefreshToken string `json:"refresh_token"`
			AccessToken  string `json:"access_token"`
			ErrorMessage string `json:"text"`
		}
		
		_, err := base.RestyClient.R().
			SetHeader("User-Agent", "Mozilla/5.0 (Macintosh; Apple macOS 15_5) AppleWebKit/537.36 (KHTML, like Gecko) Safari/537.36 Chrome/138.0.0.0 Openlist/425.6.30").
			SetResult(&resp).
			SetQueryParams(map[string]string{
				"refresh_ui": d.RefreshToken,
				"server_use": "true",
				"driver_txt": "alicloud_qr",
			}).
			Get(url)
		if err != nil {
			return "", "", err
		}
		if resp.RefreshToken == "" || resp.AccessToken == "" {
			if resp.ErrorMessage != "" {
				return "", "", fmt.Errorf("failed to refresh token: %s", resp.ErrorMessage)
			}
			return "", "", fmt.Errorf("empty token returned from official API, a wrong refresh token may have been used")
		}
		return resp.RefreshToken, resp.AccessToken, nil
	} else {
		url = d.base + "/oauth/access_token"
		if d.OauthTokenURL != "" && d.ClientID == "" {
			url = d.OauthTokenURL
		}
		//var resp base.TokenResp
		var e ErrResp
		res, err := base.RestyClient.R().
			//ForceContentType("application/json").
			SetBody(base.Json{
				"client_id":     d.ClientID,
				"client_secret": d.ClientSecret,
				"grant_type":    "refresh_token",
				"refresh_token": d.RefreshToken,
			}).
			//SetResult(&resp).
			SetError(&e).
			Post(url)
		if err != nil {
			return "", "", err
		}
		log.Debugf("[ali_open] refresh token response: %s", res.String())
		if e.Code != "" {
			return "", "", fmt.Errorf("failed to refresh token: %s", e.Message)
		}
		refresh, access := utils.Json.Get(res.Body(), "refresh_token").ToString(), utils.Json.Get(res.Body(), "access_token").ToString()
		if refresh == "" {
			return "", "", fmt.Errorf("failed to refresh token: refresh token is empty, resp: %s", res.String())
		}
		curSub, err := getSub(d.RefreshToken)
		if err != nil {
			return "", "", err
		}
		newSub, err := getSub(refresh)
		if err != nil {
			return "", "", err
		}
		if curSub != newSub {
			return "", "", errors.New("failed to refresh token: sub not match")
		}
		return refresh, access, nil
	}
}

func getSub(token string) (string, error) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return "", errors.New("not a jwt token because of invalid segments")
	}
	bs, err := base64.RawStdEncoding.DecodeString(segments[1])
	if err != nil {
		return "", errors.New("failed to decode jwt token")
	}
	return utils.Json.Get(bs, "sub").ToString(), nil
}

func (d *AliyundriveOpen) refreshToken() error {
	refresh, access, err := d._refreshToken()
	for i := 0; i < 3; i++ {
		if err == nil {
			break
		} else {
			log.Errorf("[ali_open] failed to refresh token: %s", err)
		}
		refresh, access, err = d._refreshToken()
	}
	if err != nil {
		return err
	}
	log.Infof("[ali_open] token exchange: %s -> %s", d.RefreshToken, refresh)
	d.RefreshToken, d.AccessToken = refresh, access
	op.MustSaveDriverStorage(d)
	return nil
}

func (d *AliyundriveOpen) request(uri, method string, callback base.ReqCallback, retry ...bool) ([]byte, error) {
	b, err, _ := d.requestReturnErrResp(uri, method, callback, retry...)
	return b, err
}

func (d *AliyundriveOpen) requestReturnErrResp(uri, method string, callback base.ReqCallback, retry ...bool) ([]byte, error, *ErrResp) {
	req := base.RestyClient.R()
	// TODO check whether access_token is expired
	req.SetHeader("Authorization", "Bearer "+d.AccessToken)
	if method == http.MethodPost {
		req.SetHeader("Content-Type", "application/json")
	}
	if callback != nil {
		callback(req)
	}
	var e ErrResp
	req.SetError(&e)
	res, err := req.Execute(method, d.base+uri)
	if err != nil {
		if res != nil {
			log.Errorf("[aliyundrive_open] request error: %s", res.String())
		}
		return nil, err, nil
	}
	isRetry := len(retry) > 0 && retry[0]
	if e.Code != "" {
		if !isRetry && (utils.SliceContains([]string{"AccessTokenInvalid", "AccessTokenExpired", "I400JD"}, e.Code) || d.AccessToken == "") {
			err = d.refreshToken()
			if err != nil {
				return nil, err, nil
			}
			return d.requestReturnErrResp(uri, method, callback, true)
		}
		return nil, fmt.Errorf("%s:%s", e.Code, e.Message), &e
	}
	return res.Body(), nil, nil
}

func (d *AliyundriveOpen) list(ctx context.Context, data base.Json) (*Files, error) {
	var resp Files
	_, err := d.request("/adrive/v1.0/openFile/list", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data).SetResult(&resp)
	})
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func (d *AliyundriveOpen) getFiles(ctx context.Context, fileId string) ([]File, error) {
	marker := "first"
	res := make([]File, 0)
	for marker != "" {
		if marker == "first" {
			marker = ""
		}
		data := base.Json{
			"drive_id":        d.DriveId,
			"limit":           200,
			"marker":          marker,
			"order_by":        d.OrderBy,
			"order_direction": d.OrderDirection,
			"parent_file_id":  fileId,
			//"category":              "",
			//"type":                  "",
			//"video_thumbnail_time":  120000,
			//"video_thumbnail_width": 480,
			//"image_thumbnail_width": 480,
		}
		resp, err := d.limitList(ctx, data)
		if err != nil {
			return nil, err
		}
		marker = resp.NextMarker
		res = append(res, resp.Items...)
	}
	return res, nil
}

func getNowTime() (time.Time, string) {
	nowTime := time.Now()
	nowTimeStr := nowTime.Format("2006-01-02T15:04:05.000Z")
	return nowTime, nowTimeStr
}
