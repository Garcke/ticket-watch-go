package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var keyPattern = regexp.MustCompile(`^sctp([0-9]{1,20})t[A-Za-z0-9_-]{1,200}$`)
var beijing = time.FixedZone("Beijing", 8*60*60)

type jsonPoster interface {
	json(context.Context, string, any) (map[string]any, error)
}

func sendNotice(ctx context.Context, http jsonPoster, key, title, body string) error {
	parts := keyPattern.FindStringSubmatch(key)
	if len(parts) == 0 {
		return errors.New("尚未配置 Server酱³ SendKey，请运行 ticket-watch set-key")
	}
	runes := []rune(title)
	if len(runes) > 64 {
		title = string(runes[:64])
	}
	url := "https://" + parts[1] + ".push.ft07.com/send/" + key + ".send"
	result, err := http.json(ctx, url, map[string]string{"title": title, "desp": body, "tags": "票务监控"})
	if err != nil {
		// Never log an unclassified URL-bearing transport error.
		var upstream *sourceError
		if errors.As(err, &upstream) {
			return upstream
		}
		return &sourceError{message: "Server酱³ HTTPS 请求未完成"}
	}
	code, ok := getInt(result["code"])
	if ok && code == 0 {
		return nil
	}
	message := "Server酱³ 未确认送达"
	if ok {
		message += fmt.Sprintf("（代码 %d）", code)
	}
	return &sourceError{message: message, cooldown: 120 * time.Second}
}

func safeMarkdown(s string) string {
	var out strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\`*_{}[]()<>#+.!|&~-", r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

func priceLabel(cents *int64) string {
	if cents == nil {
		return ""
	}
	return fmt.Sprintf("¥%d.%02d", *cents/100, *cents%100)
}

func buildNotice(t target, snap snapshot, pending []listing, now time.Time) (title, body string) {
	name := t.title(snap)
	escaped := safeMarkdown(name)
	if t.event.platform == damai {
		title = "【购票提醒·大麦】" + name
		body = fmt.Sprintf("**%s**\n\n公开详情显示「%s」。这是项目级购票入口，不代表某一场次或票档有余票。", escaped, safeMarkdown(pending[0].detail))
	} else {
		title = "【有票提醒·B站】" + name
		var msg strings.Builder
		fmt.Fprintf(&msg, "**%s**\n\n检测到 %d 个票档显示可购：\n", escaped, len(pending))
		for _, row := range pending[:min(25, len(pending))] {
			fmt.Fprintf(&msg, "\n- %s · %s", safeMarkdown(row.screen), safeMarkdown(row.name))
			if price := priceLabel(row.priceCents); price != "" {
				fmt.Fprintf(&msg, " · %s", price)
			}
		}
		if len(pending) > 25 {
			fmt.Fprintf(&msg, "\n- 还有 %d 个票档，请查看官方详情。", len(pending)-25)
		}
		body = msg.String()
	}
	body += fmt.Sprintf("\n\n[打开官方详情](%s)\n\n检测时间：%s（北京时间）。实际以官方页面为准。", t.event.url(), now.In(beijing).Format("2006-01-02 15:04:05"))
	return
}
