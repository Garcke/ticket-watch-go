package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Synthetic fixtures only. All HTTP calls use an in-process RoundTripper.
var (
	sourceTestBili  = event{bili, "90001"}
	sourceTestDamai = event{damai, "90002"}
	sourceTestNow   = time.Unix(1795321254, 0)
)

func TestSourceCanonicalEventURLs(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want event
	}{
		{sourceTestBili.url(), sourceTestBili},
		{" https://SHOW.BILIBILI.COM:443/platform/detail.html?project_id=90001&tracking=abc#ignore ", sourceTestBili},
		{"https://show.bilibili.com/m/platform/detail.html?projectId=90001", sourceTestBili},
		{"https://mall.bilibili.com/neul-next/ticket-renovation/detail.html?itemsId=90001", sourceTestBili},
		{"https://mall.bilibili.com/neul-next/ticket-renovation/detail.html?id=90001&ignored=a;b", sourceTestBili},
		{sourceTestDamai.url(), sourceTestDamai},
		{"https://detail.damai.cn/item.htm?itemId=90002", sourceTestDamai},
		{"https://m.damai.cn/damai/detail/item.html?itemId=90002&spm=abc", sourceTestDamai},
		{"https://show.bilibili.com/platform/detail.html?%69d=%39%39%39%39%39%39%39%39%39%39%39%39%39%39%39%39%39%39%39", event{bili, "9999999999999999999"}},
	} {
		got, err := parseEventURL(tc.raw)
		if err != nil || got != tc.want || got.url() != tc.want.url() || got.key() != tc.want.key() {
			t.Errorf("parseEventURL(%q) = %v, %v; want %v", tc.raw, got, err, tc.want)
		}
	}
	if sourceTestBili.key() != "bilibili:90001" || sourceTestDamai.key() != "damai:90002" ||
		bili.label() != "B站" || damai.label() != "大麦" {
		t.Fatal("event keys or platform labels changed")
	}
	if available.String() != "可购" || unavailable.String() != "不可购" || unknown.String() != "未知" || (listing{}).state != unknown {
		t.Fatal("availability labels or default changed")
	}
}

func TestSourceRejectsUntrustedEventURLs(t *testing.T) {
	for _, raw := range []string{
		"https://show.bilibili.com.example.invalid/platform/detail.html?id=90001",
		"https://show.bilibili.com./platform/detail.html?id=90001",
		"https://example.invalid@show.bilibili.com/platform/detail.html?id=90001",
		"https://@show.bilibili.com/platform/detail.html?id=90001",
		"https://show.bilibili.com:444/platform/detail.html?id=90001",
		"https://show.bilibili.com/platform/detail.html?id=1&id=2",
		"https://show.bilibili.com/platform/detail.html?id=1&projectId=1",
		"https://show.bilibili.com/platform/detail.html?id=1&%69d=",
		"https://show.bilibili.com/%70latform/detail.html?id=1",
		"https://show.bilibili.com/platform/detail.html/?id=1",
		"https://mall.bilibili.com/mall-search-items/items_detail/info?id=1",
		"https://detail.damai.cn/item.htm?id=0",
		"https://detail.damai.cn/item.htm?id=01",
		"https://detail.damai.cn/item.htm?id=１",
		"https://detail.damai.cn/item.htm?id=+1",
		"https://detail.damai.cn/item.htm?id=1e1",
		"https://detail.damai.cn/item.htm?id=%zz",
		"https://detail.damai.cn/item.htm?id=12345678901234567890",
		"https://m.damai.cn/damai/detail/item.html?id=1",
		"http://detail.damai.cn/item.htm?id=12",
		"//detail.damai.cn/item.htm?id=12",
		"https:detail.damai.cn/item.htm?id=12",
		"https://detail.damai.cn/item.htm#?id=12",
		"https://detail.damai.cn/item.htm?id=12" + strings.Repeat("&x=", 80),
		"https://detail.damai.cn/item.htm?id=12&tracking=" + strings.Repeat("a", 4096),
	} {
		if _, err := parseEventURL(raw); err == nil {
			t.Errorf("accepted forbidden or ambiguous URL %.110q", raw)
		}
	}
}

func TestSourceTextAndNumbers(t *testing.T) {
	if got := cleanText("  测\t试\u200b 👩\u00a0第\n四\x00次", 7); got != "测 试 👩 第" || !utf8.ValidString(got) {
		t.Errorf("cleanText = %q", got)
	}
	for _, tc := range []struct {
		value any
		want  int64
		ok    bool
	}{
		{json.Number("9223372036854775807"), 9223372036854775807, true},
		{json.Number("-9223372036854775808"), -9223372036854775808, true},
		{json.Number("9223372036854775808"), 0, false},
		{" 000 ", 0, true},
		{"\u3000-412\n", -412, true},
		{"1234567890123456789", 0, false},
		{json.Number("2.0"), 0, false},
		{json.Number("2e0"), 0, false},
		{"+2", 0, false},
		{2.0, 0, false},
		{true, 0, false},
		{nil, 0, false},
	} {
		if n, ok := getInt(tc.value); ok != tc.ok || (ok && n != tc.want) {
			t.Errorf("getInt(%#v) = %d, %t; want %d, %t", tc.value, n, ok, tc.want, tc.ok)
		}
	}
}

func TestSourceTimestampPrecision(t *testing.T) {
	want := time.Date(2026, time.December, 1, 5, 10, 0, 123456000, time.UTC)
	for _, input := range []string{
		"2026-12-01 13:10:00.123456", "2026-12-01T13:10:00,123456789+08:00",
		"2026-12-01T05:10:00.123456Z", "20261201T131000.123456+0800",
		"2026-W49-2T13:10:00.123456+08",
	} {
		if got, ok := sourceTimestamp(input); !ok || !got.Equal(want) {
			t.Errorf("timestamp %q = %v, %t; want %v", input, got, ok, want)
		}
	}
	millis := sourceTestNow.UnixMilli() + 21
	if got, ok := sourceTimestamp(json.Number(fmt.Sprint(millis))); !ok || !got.Equal(time.UnixMilli(millis)) {
		t.Errorf("millisecond timestamp = %v, %t", got, ok)
	}
	for _, input := range []any{"2026-02-29", "2026-W54-1", "2026-12-01T24:00:00", "2026-12-01T13:10:00+99:00", "0", -1, json.Number("1.0")} {
		if _, ok := sourceTimestamp(input); ok {
			t.Errorf("accepted invalid timestamp %v", input)
		}
	}
}

func sourceTestBiliReply(tickets ...any) map[string]any {
	return map[string]any{"code": 0, "success": true, "data": map[string]any{
		"projectId": 90001, "projectName": "北京·测试活动", "currentTime": sourceTestNow.Unix(),
		"screenList": []any{map[string]any{"id": 501, "name": "11月27日", "clickable": true, "ticket_list": tickets}},
	}}
}

func TestSourceBiliAvailabilityAndAliases(t *testing.T) {
	reply := sourceTestBiliReply(
		map[string]any{"id": 1, "num": 8, "sale_flag": map[string]any{"number": 3}, "clickable": false},
		map[string]any{"id": 2, "num": 8, "sale_flag": map[string]any{"number": 2, "display_name": "预售中"}, "clickable": true, "desc": " 180\t元 ", "price": 18000},
		map[string]any{"id": 3, "num": "0", "sale_flag": map[string]any{"number": 2}, "clickable": true},
		map[string]any{"id": 4, "num": 8, "clickable": true},
		map[string]any{"sku_id": "5", "stock_num": "2", "saleFlagNumber": "6", "canClick": "1", "ticket_price": -8},
		map[string]any{"id": 6, "num": 2, "sale_flag_number": 2},
		map[string]any{"id": 7, "num": 2, "sale_flag_number": 2, "clickable": true, "sale_flag": "缺货登记"},
		map[string]any{"id": 8, "saleFlag": map[string]any{"displayName": "在售"}, "canClick": " TRUE ", "skuName": "甲", "ticketPrice": 0},
		map[string]any{"id": 9, "saleFlag": map[string]any{"saleFlagNumber": 777, "displayName": "在售"}, "canClick": true},
		map[string]any{"id": 10, "saleFlag": map[string]any{"sale_flag_number": json.Number("9999999999999999999"), "displayName": "在售"}, "canClick": true},
		map[string]any{"id": 11, "saleFlag": "立即购票", "stockNum": 0, "canClick": true},
		map[string]any{"id": 12, "saleFlag": "立即购买", "canClick": json.Number("1.0")},
		map[string]any{"id": true, "saleFlagNumber": 2, "canClick": true},
		"malformed ticket",
	)
	got, err := parseBilibili(reply, sourceTestBili, sourceTestNow)
	want := []availability{unavailable, available, unavailable, unknown, available, unknown, unavailable, available, unknown, unknown, unavailable, unknown}
	if err != nil || got.name != "北京·测试活动" || len(got.listings) != len(want) {
		t.Fatalf("unexpected Bili snapshot: %+v, %v", got, err)
	}
	for i, row := range got.listings {
		if row.state != want[i] || row.screen != "11月27日" || row.screenID != "501" || row.skuID != fmt.Sprint(i+1) {
			t.Errorf("listing %d = %+v; want %s", i, row, want[i])
		}
	}
	if row := got.listings[1]; row.name != "180 元" || row.detail != "预售中" || row.priceCents == nil || *row.priceCents != 18000 {
		t.Errorf("name, detail or price lost: %+v", row)
	}
	if got.listings[4].priceCents != nil || got.listings[7].priceCents == nil || *got.listings[7].priceCents != 0 {
		t.Error("negative or zero prices misclassified")
	}
}

func TestSourceBiliWindowsAndServerClock(t *testing.T) {
	for _, tc := range []struct {
		field  string
		value  any
		screen bool
		want   availability
	}{
		{"canClick", false, true, unavailable},
		{"sale_flag_number", 4, true, unavailable},
		{"saleStart", sourceTestNow.UnixMilli() + 1, false, unavailable},
		{"sale_start", "2026-12-01 13:10:00", false, unavailable},
		{"saleStart", sourceTestNow.UnixMilli(), false, available},
		{"saleEnd", sourceTestNow.UnixMilli(), false, unavailable},
		{"sale_end", sourceTestNow.UnixMilli() + 1, true, available},
		{"saleEnd", sourceTestNow.Unix() - 5, true, unavailable},
	} {
		ticket := map[string]any{"skuId": "9", "canClick": "true", "saleFlag": map[string]any{"number": 2}, "ticketDesc": "乙"}
		screen := map[string]any{"screenId": "501", "ticketList": []any{ticket}, "start_time_str": "10:00"}
		if tc.screen {
			screen[tc.field] = tc.value
		} else {
			ticket[tc.field] = tc.value
		}
		reply := map[string]any{"errno": "0", "data": map[string]any{
			"id": "90001", "current_time": sourceTestNow.Unix(), "screen_list": []any{screen}, "project_name": "模拟",
		}}
		got, err := parseBilibili(reply, sourceTestBili, sourceTestNow.Add(2*time.Hour))
		if err != nil || got.name != "模拟" || len(got.listings) != 1 || got.listings[0].state != tc.want ||
			got.listings[0].screen != "10:00" || got.listings[0].name != "乙" {
			t.Errorf("%s=%v on screen=%t: %+v, %v; want %s", tc.field, tc.value, tc.screen, got, err, tc.want)
		}
	}
}

func sourceTestError(t *testing.T, err error, message string, cooldown time.Duration) {
	t.Helper()
	var sourceErr *sourceError
	if !errors.As(err, &sourceErr) || sourceErr.Error() != message || sourceErr.cooldown != cooldown {
		t.Fatalf("error = %v; want %q with cooldown %s", err, message, cooldown)
	}
}

func TestSourceBiliErrorsAndEmptyDetails(t *testing.T) {
	for _, tc := range []struct {
		reply    map[string]any
		message  string
		cooldown time.Duration
	}{
		{map[string]any{"code": -352, "message": "ignore upstream text"}, "B站商详未成功（代码 -352）", 900 * time.Second},
		{map[string]any{"code": 0, "errno": "221", "success": true}, "B站商详未成功（代码 221）", 120 * time.Second},
		{map[string]any{"code": 0, "success": false}, "B站商详未返回成功状态", 0},
		{map[string]any{"code": "not a number", "success": true}, "B站商详未返回成功状态", 0},
		{map[string]any{"success": "true"}, "B站商详未返回成功状态", 0},
		{map[string]any{"code": 0, "data": []any{}}, "B站商详没有活动数据", 0},
		{map[string]any{"code": 0, "data": map[string]any{}}, "B站没有公开场次列表", 0},
		{map[string]any{"code": 0, "data": map[string]any{"project_id": 1, "screenList": []any{}}}, "B站返回的活动编号与请求不一致", 0},
		{sourceTestBiliReply(map[string]any{"id": 3}, map[string]any{"sku_id": "3"}), "B站返回重复票档，未更新查票状态", 0},
	} {
		_, err := parseBilibili(tc.reply, sourceTestBili, sourceTestNow)
		sourceTestError(t, err, tc.message, tc.cooldown)
	}
	if got, err := parseBilibili(sourceTestBiliReply(), sourceTestBili, sourceTestNow); err != nil || len(got.listings) != 0 {
		t.Fatalf("empty detail asserted inventory or errored: %+v, %v", got, err)
	}
}

func sourceTestDamaiPage(code any, shown, button string, extra map[string]any) string {
	obj := map[string]any{"buyBtnStatus": code, "buyBtnShow": shown, "buyBtnText": button}
	for k, v := range extra {
		obj[k] = v
	}
	data, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return "<html><title>【长沙】音乐会 &amp; 演出【网上订票】- 大麦网</title>" +
		"<div id='dataDefault' style='display: none'>" + string(data) + "</div></html>"
}

func TestSourceDamaiProjectLevelStatus(t *testing.T) {
	for _, tc := range []struct {
		code          any
		shown, button string
		extra         map[string]any
		want          availability
	}{
		{204, "立即购票", "立即购票", nil, available},
		{"204", "特惠购票", "", nil, available},
		{206, "选座购票", "选座购买", nil, available},
		{91, "", "缺货登记", nil, unavailable},
		{204, "立即购票", "候补", nil, unavailable},
		{100, "", "该渠道不支持购票", nil, unknown},
		{777, "立即购票", "立即购票", nil, unknown},
		{204, "", "立即购票", nil, unknown},
		{206, "立即购票", "", nil, unknown},
		{204, "立即购票", "立即购票", map[string]any{"appNeedUpGrade": "1"}, unknown},
		{204, "立即购票", "立即购票", map[string]any{"sellStartTime": sourceTestNow.UnixMilli() + 1}, unavailable},
		{204, "立即购票", "立即购票", map[string]any{"sellStartTime": "invalid", "sellStartTimeStr": "2026-12-01 13:10:00"}, unavailable},
		{204, "立即购票", "立即购票", map[string]any{"sellStartTime": sourceTestNow.UnixMilli(), "skuId": 123, "price": 8000}, available},
	} {
		got, err := parseDamai(sourceTestDamaiPage(tc.code, tc.shown, tc.button, tc.extra), sourceTestDamai, sourceTestNow)
		if err != nil || got.name != "【长沙】音乐会 & 演出" || len(got.listings) != 1 {
			t.Fatalf("Damai snapshot = %+v, %v", got, err)
		}
		row := got.listings[0]
		wantDetail := tc.shown
		if wantDetail == "" {
			wantDetail = tc.button
		}
		if row.state != tc.want || row.name != "公开购票按钮" || row.detail != wantDetail || row.screen != "" ||
			row.screenID != "" || row.skuID != "" || row.priceCents != nil {
			t.Errorf("Damai code=%v: listing = %+v; want project-level status %v", tc.code, row, tc.want)
		}
	}
}

func TestSourceDamaiHTMLAndErrors(t *testing.T) {
	encoded := `<DIV ID = "dataDefault">{&quot;buyBtnStatus&quot;:204,&quot;buyBtnShow&quot;:&quot;立即购票&quot;}</DIV>`
	if got, err := parseDamai(encoded, sourceTestDamai, sourceTestNow); err != nil || got.name != "大麦活动 90002" ||
		len(got.listings) != 1 || got.listings[0].state != available {
		t.Errorf("entity-encoded Damai snapshot = %+v, %v", got, err)
	}
	for _, tc := range []struct {
		page, message string
		cooldown      time.Duration
	}{
		{"<html>CAPTCHA / 访问验证</html>", "大麦详情要求验证", 900 * time.Second},
		{"<html>no public data</html>", "大麦公开详情页没有返回售卖数据", 0},
		{`<div data-id="dataDefault">{}</div>`, "大麦公开详情页没有返回售卖数据", 0},
		{`<div id="dataDefault">{broken}</div>`, "大麦公开售卖数据无法解析", 0},
		{`<div id="dataDefault">{"unrelated":true}</div>`, "大麦公开售卖数据结构变化", 0},
	} {
		_, err := parseDamai(tc.page, sourceTestDamai, sourceTestNow)
		sourceTestError(t, err, tc.message, tc.cooldown)
	}
}

type sourceTestTransport func(*http.Request) (*http.Response, error)

func (fn sourceTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func sourceTestResponse(code int, header http.Header, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func TestSourceOfficialHTTPRequestShapes(t *testing.T) {
	largeID := "9999999999999999999"
	biliEvent := event{bili, largeID}
	calls := 0
	w := newWebClient()
	w.client.Transport = sourceTestTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("User-Agent") != "TicketWatch/1.0" || req.Header.Get("Cookie") != "" || req.Header.Get("Authorization") != "" {
			t.Fatalf("wrong or credential-bearing request headers: %v", req.Header)
		}
		deadline, ok := req.Context().Deadline()
		if !ok || time.Until(deadline) > 8*time.Second || !deadline.After(time.Now()) {
			t.Fatalf("unexpected request deadline: %v, %t", deadline, ok)
		}
		switch calls {
		case 1:
			if req.Method != http.MethodPost || req.URL.String() != biliMall || req.Header.Get("Accept") != "application/json" ||
				req.Header.Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("unexpected Bili request: %s %s %v", req.Method, req.URL, req.Header)
			}
			defer req.Body.Close()
			raw, err := io.ReadAll(req.Body)
			if err != nil || string(raw) != `{"itemsDetailPageType":3,"itemsId":9999999999999999999}` {
				t.Fatalf("unexpected Bili payload: %s, %v", raw, err)
			}
			return sourceTestResponse(200, nil, `{"success":true,"data":{"project_id":`+largeID+`,"screen_list":[]}}`), nil
		case 2:
			if req.Method != http.MethodGet || req.URL.String() != sourceTestDamai.url() || req.Header.Get("Accept") != "text/html" ||
				req.Header.Get("Content-Type") != "" || (req.Body != nil && req.Body != http.NoBody) {
				t.Fatalf("unexpected Damai request: %s %s %v", req.Method, req.URL, req.Header)
			}
			return sourceTestResponse(200, nil, sourceTestDamaiPage(204, "立即购票", "", nil)), nil
		default:
			t.Fatal("request made for invalid event")
			return nil, errors.New("unexpected request")
		}
	})
	if got, err := w.query(context.Background(), biliEvent); err != nil || got.name != "会员购活动 "+largeID || len(got.listings) != 0 {
		t.Fatalf("Bili query = %+v, %v", got, err)
	}
	if got, err := w.query(context.Background(), sourceTestDamai); err != nil || len(got.listings) != 1 || got.listings[0].state != available {
		t.Fatalf("Damai query = %+v, %v", got, err)
	}
	for _, e := range []event{{damai, "1&other=2"}, {platform("other"), "1"}} {
		if _, err := w.query(context.Background(), e); err == nil {
			t.Errorf("query accepted invalid event %v", e)
		}
	}
	if calls != 2 {
		t.Errorf("received %d requests, want two", calls)
	}
}

func TestSourceHTTPTimeoutTLSAndNoProxy(t *testing.T) {
	w := newWebClient()
	tr, ok := w.client.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || tr.TLSClientConfig == nil || tr.TLSClientConfig.InsecureSkipVerify || w.client.Jar != nil ||
		w.client.Timeout != 8*time.Second || w.client.CheckRedirect == nil || w.client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("client must verify TLS, disable redirects, proxies and cookies, and use an eight-second timeout")
	}
}

func TestSourceHTTPCooldownAndRedirect(t *testing.T) {
	for _, tc := range []struct {
		code           int
		retry, message string
		cooldown       time.Duration
	}{
		{302, "321", "HTTP 302：拒绝访问或要求跳转/验证", 900 * time.Second},
		{307, "901", "HTTP 307：拒绝访问或要求跳转/验证", 901 * time.Second},
		{401, "", "HTTP 401：拒绝访问或要求跳转/验证", 900 * time.Second},
		{403, "", "HTTP 403：拒绝访问或要求跳转/验证", 900 * time.Second},
		{406, "", "HTTP 406：拒绝访问或要求跳转/验证", 900 * time.Second},
		{412, "", "HTTP 412：拒绝访问或要求跳转/验证", 900 * time.Second},
		{418, "", "HTTP 418：拒绝访问或要求跳转/验证", 900 * time.Second},
		{429, "321", "HTTP 429：限流", 321 * time.Second},
		{429, "1", "HTTP 429：限流", 120 * time.Second},
		{503, "180", "HTTP 503", 180 * time.Second},
	} {
		for _, location := range []string{"https://redirect.example.invalid/do-not-fetch", "https://%zz/invalid-location"} {
			w := newWebClient()
			calls := 0
			w.client.Transport = sourceTestTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls != 1 {
					t.Fatal("followed Location")
				}
				return sourceTestResponse(tc.code, http.Header{"Retry-After": {tc.retry}, "Location": {location}}, "mock-private-data"), nil
			})
			_, err := w.json(context.Background(), "https://notify.example.invalid/mock-private-path", map[string]any{})
			sourceTestError(t, err, tc.message, tc.cooldown)
			if calls != 1 {
				t.Fatalf("received %d requests", calls)
			}
		}
	}
	if got := sourceRetryAfter("Thu, 01 Jan 1970 00:01:00 GMT", time.Unix(20, 0)); got != 40*time.Second {
		t.Errorf("HTTP date cooldown = %v", got)
	}
	if got := sourceRetryAfter("Thu, 01 Jan 1970 00:01:00 GMT", time.Unix(20, 500_000_000)); got != 40*time.Second {
		t.Errorf("HTTP date cooldown rounded down: %v", got)
	}
	if got := sourceRetryAfter("999999999999", sourceTestNow); got <= 0 {
		t.Errorf("numeric Retry-After overflowed: %v", got)
	}
	for _, value := range []string{"malformed", "-42", "Thu, 01 Jan 1970 00:01:00 GMT"} {
		if got := sourceRetryAfter(value, sourceTestNow); got != 0 {
			t.Errorf("expired or invalid Retry-After %q = %v", value, got)
		}
	}
}

type sourceTestReadFailure struct{}

func (sourceTestReadFailure) Read([]byte) (int, error) { return 0, errors.New("mock-private-path") }
func (sourceTestReadFailure) Close() error             { return nil }

func TestSourceHTTPResponseAndSecretSafety(t *testing.T) {
	endpoint := "https://notify.example.invalid/mock-private-path"
	for _, tc := range []struct {
		body, message string
		cooldown      time.Duration
	}{
		{" \n<html>mock-private-path</html>", "接口返回网页而不是数据，可能需要验证", 900 * time.Second},
		{`{"key": mock-private-path}`, "接口没有返回有效 JSON", 0},
		{`{"code":0} {"extra":true}`, "接口没有返回有效 JSON", 0},
		{`[]`, "接口未返回预期的数据结构", 0},
		{`null`, "接口未返回预期的数据结构", 0},
		{"\xff", "响应不是有效的 UTF-8", 0},
		{strings.Repeat("x", sourceResponseLimit+1), "响应超过大小限制", 0},
	} {
		w := newWebClient()
		w.client.Transport = sourceTestTransport(func(*http.Request) (*http.Response, error) {
			return sourceTestResponse(200, nil, tc.body), nil
		})
		_, err := w.json(context.Background(), endpoint, nil)
		sourceTestError(t, err, tc.message, tc.cooldown)
	}
	w := newWebClient()
	w.client.Transport = sourceTestTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("TLS failure at " + endpoint)
	})
	_, err := w.json(context.Background(), endpoint, nil)
	sourceTestError(t, err, sourceConnFailure, 0)
	w.client.Transport = sourceTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: sourceTestReadFailure{}}, nil
	})
	_, err = w.json(context.Background(), endpoint, nil)
	sourceTestError(t, err, sourceConnFailure, 0)
	for _, invalid := range []string{"https://%zz/mock-private-path", "https://user:mock-private-path@notify.example.invalid/", "http://notify.example.invalid/mock-private-path"} {
		w.client.Transport = sourceTestTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid endpoint reached transport")
			return nil, errors.New("invalid endpoint")
		})
		_, err = w.json(context.Background(), invalid, nil)
		sourceTestError(t, err, sourceConnFailure, 0)
	}
}

func TestSourceHTTPBOMUseNumberAndNoCookies(t *testing.T) {
	const endpoint = "https://notify.example.invalid/synthetic"
	w := newWebClient()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "synthetic-before"}})
	w.client.Jar = jar
	const payload = "\uFEFF" + `{"code":0,"id":1234567890123456789}`
	w.client.Transport = sourceTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Cookie") != "" {
			t.Fatal("sent a cookie from the injected jar")
		}
		return sourceTestResponse(200, http.Header{"Set-Cookie": {"other=synthetic-after"}},
			payload+strings.Repeat(" ", sourceResponseLimit-len(payload))), nil
	})
	got, err := w.json(context.Background(), endpoint, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := got["id"].(json.Number); !ok || string(n) != "1234567890123456789" {
		t.Errorf("ID type/precision changed: %#v", got["id"])
	}
	if n, ok := getInt(got["code"]); !ok || n != 0 {
		t.Errorf("could not read notification response code: %v", got["code"])
	}
	if cookies := jar.Cookies(u); len(cookies) != 1 || cookies[0].Name != "session" {
		t.Fatalf("persisted response cookies: %v", cookies)
	}
}
