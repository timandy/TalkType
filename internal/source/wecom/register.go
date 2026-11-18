package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// 扫码创建智能机器人：企业微信官方命令行工具 wecom-cli（auth init）用的接口，没有公开文档。
// 流程取自企业微信扫码页面 aibotQuickCreate/gen 的脚本：
//
//	GET /ai/qc/generate?source=...&plat=1   → data.scode、data.auth_url
//	浏览器打开 /ai/qc/gen?source=...&scode=  → 页面显示二维码，用企业微信扫码
//	GET /ai/qc/query_result?scode=          → data.status；为 success 时 data.bot_info 里有 botid、secret
//
// 页面每 3 秒轮询一次，二维码 5 分钟后过期。

const (
	registrationSource = "wecom_cli_external"
	scanPollInterval   = 3 * time.Second
	scanExpire         = 5 * time.Minute
)

// errScanRestart 表示本次二维码已过期，需要重新获取。
var errScanRestart = errors.New("二维码已过期")

type generateResp struct {
	Data struct {
		SCode   string `json:"scode"`
		AuthURL string `json:"auth_url"`
	} `json:"data"`
}

type queryResp struct {
	Data struct {
		Status  string `json:"status"`
		BotInfo *struct {
			BotID  string `json:"botid"`
			Secret string `json:"secret"`
		} `json:"bot_info"`
	} `json:"data"`
}

// scan 扫码创建机器人，直到成功或 ctx 结束；二维码过期时自动重新获取。
func (c *Client) scan(ctx context.Context) (botID, secret string, err error) {
	for {
		botID, secret, err = c.scanOnce(ctx)
		if errors.Is(err, errScanRestart) {
			c.logger.Println("二维码已过期，重新获取")
			continue
		}
		return botID, secret, err
	}
}

func (c *Client) scanOnce(ctx context.Context) (string, string, error) {
	q := url.Values{"source": {registrationSource}, "plat": {"1"}}
	var g generateResp
	if err := c.getJSON(ctx, c.qcBase+"/ai/qc/generate?"+q.Encode(), &g); err != nil {
		return "", "", fmt.Errorf("获取二维码失败: %w", err)
	}
	if g.Data.SCode == "" {
		return "", "", errors.New("获取二维码失败：响应中没有 scode")
	}
	page := c.qcBase + "/ai/qc/gen?" + url.Values{"source": {registrationSource}, "scode": {g.Data.SCode}}.Encode()
	c.logger.Printf("请用企业微信扫描该页面上的二维码，创建并接入智能机器人：\n%s", page)
	if c.cfg.Login.AutoOpenQR {
		c.openURL(page)
	}

	deadline := time.Now().Add(scanExpire)
	lastStatus := ""
	for time.Now().Before(deadline) {
		if err := c.sleep(ctx, scanPollInterval); err != nil {
			return "", "", err
		}
		var r queryResp
		if err := c.getJSON(ctx, c.qcBase+"/ai/qc/query_result?"+url.Values{"scode": {g.Data.SCode}}.Encode(), &r); err != nil {
			if ctx.Err() != nil {
				return "", "", ctx.Err()
			}
			c.logger.Printf("查询扫码状态失败: %v", err)
			continue
		}
		if r.Data.Status == "success" && r.Data.BotInfo != nil && r.Data.BotInfo.BotID != "" && r.Data.BotInfo.Secret != "" {
			return r.Data.BotInfo.BotID, r.Data.BotInfo.Secret, nil
		}
		if s := r.Data.Status; s != lastStatus && s != "init" && s != "" {
			c.logger.Printf("扫码状态: %s", s)
		}
		lastStatus = r.Data.Status
	}
	return "", "", errScanRestart
}

func (c *Client) getJSON(ctx context.Context, rawURL string, out any) error {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	return nil
}
