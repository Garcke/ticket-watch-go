package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type platform string

const (
	bili  platform = "bilibili"
	damai platform = "damai"
)

func (p platform) label() string {
	switch p {
	case bili:
		return "B站"
	case damai:
		return "大麦"
	default:
		return ""
	}
}

type event struct {
	platform platform
	id       string
}

func (e event) key() string { return string(e.platform) + ":" + e.id }

func (e event) url() string {
	switch e.platform {
	case bili:
		return "https://show.bilibili.com/platform/detail.html?id=" + e.id
	case damai:
		return "https://detail.damai.cn/item.htm?id=" + e.id
	default:
		return ""
	}
}

type availability uint8

const (
	unknown availability = iota // The zero value never asserts availability.
	available
	unavailable
)

func (a availability) String() string {
	switch a {
	case available:
		return "可购"
	case unavailable:
		return "不可购"
	default:
		return "未知"
	}
}

type listing struct {
	screenID, skuID, screen, name string
	state                         availability
	detail                        string
	priceCents                    *int64
}

type snapshot struct {
	name     string
	listings []listing
}

type sourceError struct {
	message  string
	cooldown time.Duration
}

func (e *sourceError) Error() string { return e.message }

var eventURLRules = map[[2]string]struct {
	platform platform
	idKeys   []string
}{
	{"show.bilibili.com", "/platform/detail.html"}:                    {bili, []string{"id", "project_id", "projectId"}},
	{"show.bilibili.com", "/m/platform/detail.html"}:                  {bili, []string{"id", "project_id", "projectId"}},
	{"mall.bilibili.com", "/neul-next/ticket-renovation/detail.html"}: {bili, []string{"id", "itemsId"}},
	{"detail.damai.cn", "/item.htm"}:                                  {damai, []string{"id", "itemId"}},
	{"m.damai.cn", "/damai/detail/item.html"}:                         {damai, []string{"itemId"}},
}

func parseEventURL(raw string) (event, error) {
	if utf8.RuneCountInString(raw) > 4096 {
		return event{}, errors.New("请填写官方活动详情链接")
	}
	badURL := errors.New("链接须为含项目编号的 B 站会员购或大麦官方活动详情页；短链接请先在浏览器展开")
	trimmed, _, _ := strings.Cut(trimSourceSpace(raw), "#") // Never retain the fragment.
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" {
		return event{}, badURL
	}
	if port := u.Port(); port != "" && strings.TrimLeft(port, "0") != "443" {
		return event{}, badURL
	}
	rule, ok := eventURLRules[[2]string{strings.ToLower(u.Hostname()), u.EscapedPath()}]
	if !ok || strings.Count(u.RawQuery, "&") >= 80 {
		return event{}, badURL
	}
	var id string
	// Decode only the allowlisted fields. Tracking parameters are not forwarded.
	for _, param := range strings.Split(u.RawQuery, "&") {
		key, value, _ := strings.Cut(param, "=")
		key, err := url.QueryUnescape(key)
		if err != nil {
			continue
		}
		for _, alias := range rule.idKeys {
			if key != alias {
				continue
			}
			candidate, err := url.QueryUnescape(value)
			if id != "" || err != nil || !validID(candidate) {
				return event{}, badURL
			}
			id = candidate
		}
	}
	if id == "" {
		return event{}, badURL
	}
	return event{platform: rule.platform, id: id}, nil
}

func validID(s string) bool {
	if len(s) == 0 || len(s) > 19 || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func sourceID(value any) string {
	var s string
	switch v := value.(type) {
	case string:
		s = v
	case json.Number:
		s = string(v) // IDs may have 19 digits and exceed int64.
	case int:
		s = strconv.Itoa(v)
	case int64:
		s = strconv.FormatInt(v, 10)
	}
	if validID(s) {
		return s
	}
	return ""
}

func sourceSpace(r rune) bool         { return unicode.IsSpace(r) || (r >= '\u001c' && r <= '\u001f') }
func trimSourceSpace(s string) string { return strings.TrimFunc(s, sourceSpace) }

func cleanText(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	text := strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || !unicode.IsPrint(r)
	}), " ")
	for i := range text {
		if limit == 0 {
			return text[:i]
		}
		limit--
	}
	return text
}

func sourceText(value any, limit int) string {
	s, _ := value.(string)
	return cleanText(s, limit)
}

func sourceFirst(obj map[string]any, keys ...string) any {
	for _, key := range keys {
		if v := obj[key]; v != nil && v != "" {
			return v
		}
	}
	return nil
}

var sourceIntString = regexp.MustCompile(`^-?[0-9]{1,18}$`)

// getInt accepts integers, not float or bool conversions. JSON is decoded with UseNumber.
func getInt(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		s := trimSourceSpace(v)
		if sourceIntString.MatchString(s) {
			n, err := strconv.ParseInt(s, 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

func sourceBool(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(trimSourceSpace(v)) {
		case "1", "true":
			return true, true
		case "0", "false":
			return false, true
		}
	default:
		if n, ok := getInt(v); ok && (n == 0 || n == 1) {
			return n == 1, true
		}
	}
	return false, false
}

var (
	sourceBeijing  = time.FixedZone("Beijing", 8*60*60)
	sourceISODate  = regexp.MustCompile(`^([0-9]{4})(?:-([0-9]{2})-([0-9]{2})|([0-9]{2})([0-9]{2})|-W([0-9]{2})(?:-([0-9]))?|W([0-9]{2})([0-9])?)`)
	sourceISOClock = regexp.MustCompile(`^([0-9]{2})(?::([0-9]{2})(?::([0-9]{2}))?|([0-9]{2})([0-9]{2})?)?(?:[.,]([0-9]+))?$`)
)

// A sale time is compared at its original precision, including Unix milliseconds
// and the microseconds supported by Python's datetime.fromisoformat.
func sourceTimestamp(value any) (time.Time, bool) {
	if n, ok := getInt(value); ok {
		switch {
		case n <= 0:
			return time.Time{}, false
		case n >= 100_000_000_000:
			return time.UnixMilli(n), true
		default:
			return time.Unix(n, 0), true
		}
	}
	if n, ok := value.(json.Number); ok && validPositiveJSONInteger(string(n)) {
		// A much-larger-than-int64 epoch is still in the future for this application.
		return time.UnixMilli(1<<63 - 1), true
	}
	s, ok := value.(string)
	if !ok {
		return time.Time{}, false
	}
	return sourceISOTimestamp(trimSourceSpace(s))
}

func validPositiveJSONInteger(s string) bool {
	return len(s) > 0 && s[0] >= '1' && s[0] <= '9' && strings.Trim(s, "0123456789") == ""
}

func sourceClock(s string) (time.Duration, bool) {
	parts := sourceISOClock.FindStringSubmatch(s)
	if parts == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(parts[1])
	m, _ := strconv.Atoi(parts[2] + parts[4]) // Only one spelling is matched.
	sec, _ := strconv.Atoi(parts[3] + parts[5])
	if h > 23 || m > 59 || sec > 59 {
		return 0, false
	}
	frac := parts[6]
	if len(frac) > 6 {
		frac = frac[:6]
	}
	micros, _ := strconv.Atoi(frac + "000000"[:6-len(frac)])
	return time.Duration(h*3600+m*60+sec)*time.Second + time.Duration(micros)*time.Microsecond, true
}

func sourceISOTimestamp(s string) (time.Time, bool) {
	parts := sourceISODate.FindStringSubmatch(s)
	if parts == nil {
		return time.Time{}, false
	}
	year, _ := strconv.Atoi(parts[1])
	if year == 0 {
		return time.Time{}, false
	}
	var day time.Time
	if weekText := parts[6] + parts[8]; weekText != "" {
		week, _ := strconv.Atoi(weekText)
		weekday := 1
		if text := parts[7] + parts[9]; text != "" {
			weekday, _ = strconv.Atoi(text)
		}
		if week < 1 || week > 53 || weekday < 1 || weekday > 7 {
			return time.Time{}, false
		}
		jan4 := time.Date(year, time.January, 4, 0, 0, 0, 0, time.UTC)
		day = jan4.AddDate(0, 0, (week-1)*7+weekday-1-(int(jan4.Weekday())+6)%7)
		if y, w := day.ISOWeek(); y != year || w != week || day.Year() > 9999 {
			return time.Time{}, false
		}
	} else {
		month, _ := strconv.Atoi(parts[2] + parts[4])
		d, _ := strconv.Atoi(parts[3] + parts[5])
		day = time.Date(year, time.Month(month), d, 0, 0, 0, 0, time.UTC)
		if day.Year() != year || int(day.Month()) != month || day.Day() != d {
			return time.Time{}, false
		}
	}
	clock := s[len(parts[0]):]
	if clock == "" {
		return day.Add(-8 * time.Hour), true
	}
	_, sepLen := utf8.DecodeRuneInString(clock)
	clock = clock[sepLen:]
	var zone string
	if i := strings.IndexAny(clock, "Z+-"); i >= 0 {
		clock, zone = clock[:i], clock[i:]
	}
	at, ok := sourceClock(clock)
	if !ok {
		return time.Time{}, false
	}
	switch zone {
	case "":
		_, offset := day.In(sourceBeijing).Zone()
		return day.Add(at - time.Duration(offset)*time.Second), true
	case "Z":
		return day.Add(at), true
	}
	if zone[0] != '+' && zone[0] != '-' {
		return time.Time{}, false
	}
	offset, ok := sourceClock(zone[1:])
	if !ok {
		return time.Time{}, false
	}
	if zone[0] == '-' {
		offset = -offset
	}
	return day.Add(at - offset), true
}

func sourceSaleTime(obj map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if t, ok := sourceTimestamp(obj[key]); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

const (
	biliMall                   = "https://mall.bilibili.com/mall-search-items/items_detail/info"
	sourceResponseLimit        = 2_000_000
	sourceHTTPTimeout          = 8 * time.Second
	sourceRateLimitCooldown    = 120 * time.Second
	sourceVerificationCooldown = 900 * time.Second
	sourceMaxCooldown          = time.Duration(1<<63 - 1)
	sourceConnFailure          = "HTTPS 连接或读取失败"
)

var sourceRetrySeconds = regexp.MustCompile(`^[0-9]{1,12}$`)

func sourceRetryAfter(value string, now time.Time) time.Duration {
	s := trimSourceSpace(value)
	if sourceRetrySeconds.MatchString(s) {
		n, _ := strconv.ParseInt(s, 10, 64)
		if n > int64(sourceMaxCooldown/time.Second) {
			return sourceMaxCooldown
		}
		return time.Duration(n) * time.Second
	}
	when, err := http.ParseTime(s)
	if err != nil {
		when, err = mail.ParseDate(s)
	}
	if err != nil || !when.After(now) {
		return 0
	}
	wait := when.Sub(now)
	if rem := wait % time.Second; rem != 0 {
		if extra := time.Second - rem; wait <= sourceMaxCooldown-extra {
			return wait + extra
		}
		return sourceMaxCooldown
	}
	return wait
}

func sourceHTTPStatusError(code int, retryAfter string, now time.Time) *sourceError {
	wait := sourceRetryAfter(retryAfter, now)
	switch {
	case code == http.StatusTooManyRequests:
		return &sourceError{"HTTP 429：限流", max(wait, sourceRateLimitCooldown)}
	case code >= 300 && code < 400 || code == 401 || code == 403 || code == 406 || code == 412 || code == 418:
		return &sourceError{fmt.Sprintf("HTTP %d：拒绝访问或要求跳转/验证", code), max(wait, sourceVerificationCooldown)}
	default:
		return &sourceError{fmt.Sprintf("HTTP %d", code), wait}
	}
}

type webClient struct {
	client *http.Client
}

func rejectSourceRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func newWebClient() *webClient {
	return &webClient{client: &http.Client{
		Transport: &http.Transport{
			// No proxy, environment lookup, browser state, or credential headers.
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:   true,
			DisableCompression:  true,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: sourceHTTPTimeout,
		},
		Timeout:       sourceHTTPTimeout,
		CheckRedirect: rejectSourceRedirect,
	}}
}

// net/http parses Location before CheckRedirect. Hide it so even a malformed
// redirect yields the original HTTP status and its cooldown, never a new request.
type sourceNoRedirectTransport struct{ http.RoundTripper }

func (t sourceNoRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.RoundTripper.RoundTrip(req)
	if res == nil || err != nil || res.StatusCode < 300 || res.StatusCode >= 400 || res.Header.Get("Location") == "" {
		return res, err
	}
	copy := *res
	copy.Header = res.Header.Clone()
	copy.Header.Del("Location")
	return &copy, nil
}

func (w *webClient) text(ctx context.Context, rawURL string, body []byte, accept string) (string, error) {
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil || req.URL.Scheme != "https" || req.URL.Hostname() == "" || req.URL.User != nil || req.URL.Opaque != "" ||
		w == nil || w.client == nil || w.client.Transport == nil {
		return "", &sourceError{message: sourceConnFailure}
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "TicketWatch/1.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	client := *w.client
	client.Jar = nil
	client.Timeout = sourceHTTPTimeout
	client.CheckRedirect = rejectSourceRedirect
	client.Transport = sourceNoRedirectTransport{client.Transport}
	res, err := client.Do(req)
	if err != nil {
		// Never return net/http errors: a notification key can be in the URL path.
		return "", &sourceError{message: sourceConnFailure}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", sourceHTTPStatusError(res.StatusCode, res.Header.Get("Retry-After"), time.Now())
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, sourceResponseLimit+1))
	if err != nil {
		return "", &sourceError{message: sourceConnFailure}
	}
	if len(raw) > sourceResponseLimit {
		return "", &sourceError{message: "响应超过大小限制"}
	}
	if !utf8.Valid(raw) {
		return "", &sourceError{message: "响应不是有效的 UTF-8"}
	}
	return strings.TrimPrefix(string(raw), "\uFEFF"), nil
}

func decodeSourceJSON(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var value, extra any
	if err := dec.Decode(&value); err != nil || dec.Decode(&extra) != io.EOF {
		return nil, false
	}
	return value, true
}

func (w *webClient) json(ctx context.Context, endpoint string, body any) (map[string]any, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		return nil, &sourceError{message: "请求数据无法编码"}
	}
	raw, err := w.text(ctx, endpoint, bytes.TrimSuffix(buf.Bytes(), []byte("\n")), "application/json")
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(trimSourceSpace(raw), "<") {
		return nil, &sourceError{"接口返回网页而不是数据，可能需要验证", sourceVerificationCooldown}
	}
	value, ok := decodeSourceJSON(raw)
	if !ok {
		return nil, &sourceError{message: "接口没有返回有效 JSON"}
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, &sourceError{message: "接口未返回预期的数据结构"}
	}
	return obj, nil
}

var sourceBlockedLabels = []string{
	"售罄", "售完", "缺货", "无票", "无货", "停售", "停止售票", "不可售", "不可购",
	"未开售", "未发售", "即将开售", "即将发售", "待开售", "已结束", "已取消",
	"下架", "登记", "候补", "无购买资格",
}

func sourceBlockedLabel(s string) bool {
	for _, label := range sourceBlockedLabels {
		if strings.Contains(s, label) {
			return true
		}
	}
	return false
}

func biliSaleNumber(value any) (int64, bool) {
	if n, ok := getInt(value); ok {
		return n, true
	}
	// An out-of-range JSON integer is an explicit, unrecognized flag, not
	// permission to fall back to a positive text label.
	if n, ok := value.(json.Number); ok && validPositiveJSONInteger(strings.TrimPrefix(string(n), "-")) {
		return 0, true
	}
	return 0, false
}

func biliFlag(item map[string]any) (n int64, hasNumber bool, label string) {
	raw := sourceFirst(item, "sale_flag", "saleFlag")
	if obj, ok := raw.(map[string]any); ok {
		n, hasNumber = biliSaleNumber(sourceFirst(obj, "number", "sale_flag_number", "saleFlagNumber"))
		label = sourceText(sourceFirst(obj, "display_name", "displayName"), 160)
	} else {
		n, hasNumber = biliSaleNumber(raw)
		if !hasNumber {
			label = sourceText(raw, 160)
		}
	}
	if !hasNumber {
		n, hasNumber = biliSaleNumber(sourceFirst(item, "sale_flag_number", "saleFlagNumber"))
	}
	return
}

func biliBlocked(item map[string]any, now time.Time, flag int64, label string) bool {
	switch flag {
	case 1, 3, 4, 5, 7, 8, 9, 101, 102, 103, 105, 106:
		return true
	}
	if sourceBlockedLabel(label) {
		return true
	}
	if canClick, known := sourceBool(sourceFirst(item, "clickable", "canClick")); known && !canClick {
		return true
	}
	start, hasStart := sourceSaleTime(item, "saleStart", "sale_start")
	end, hasEnd := sourceSaleTime(item, "saleEnd", "sale_end")
	return (hasStart && now.Before(start)) || (hasEnd && !now.Before(end))
}

func biliOnLabel(label string) bool {
	switch label {
	case "立即购买", "立即购票", "预售", "预售中", "售票中", "售卖中", "在售", "库存紧张":
		return true
	}
	return false
}

func parseBilibili(reply map[string]any, e event, now time.Time) (snapshot, error) {
	var code int64
	var hasCode, invalidCode bool
	for _, key := range []string{"code", "errno"} {
		if value := reply[key]; value != nil {
			hasCode = true
			n, ok := getInt(value)
			invalidCode = invalidCode || !ok
			if ok && code == 0 {
				code = n
			}
		}
	}
	success, hasSuccess := reply["success"].(bool)
	if invalidCode || code != 0 || (hasSuccess && !success) || (!hasCode && !success) {
		if code == 0 {
			return snapshot{}, &sourceError{message: "B站商详未返回成功状态"}
		}
		var wait time.Duration
		switch code {
		case -101, -352, -412, 100044:
			wait = sourceVerificationCooldown
		case 3, 221, 429, 900001, 900002:
			wait = sourceRateLimitCooldown
		}
		return snapshot{}, &sourceError{fmt.Sprintf("B站商详未成功（代码 %d）", code), wait}
	}
	data, ok := reply["data"].(map[string]any)
	if !ok {
		return snapshot{}, &sourceError{message: "B站商详没有活动数据"}
	}
	if id := sourceFirst(data, "projectId", "project_id", "id"); id != nil && sourceID(id) != e.id {
		return snapshot{}, &sourceError{message: "B站返回的活动编号与请求不一致"}
	}
	if serverNow, ok := sourceSaleTime(data, "currentTime", "current_time"); ok && !serverNow.Equal(time.Unix(0, 0)) {
		now = serverNow
	}
	name := sourceText(sourceFirst(data, "projectName", "project_name", "name"), 160)
	if name == "" {
		name = "会员购活动 " + e.id
	}
	screens, ok := sourceFirst(data, "screenList", "screen_list").([]any)
	if !ok {
		return snapshot{}, &sourceError{message: "B站没有公开场次列表"}
	}
	rows := make([]listing, 0)
	seen := make(map[[2]string]struct{})
	for _, rawScreen := range screens {
		screen, ok := rawScreen.(map[string]any)
		if !ok {
			continue
		}
		sid := sourceID(sourceFirst(screen, "id", "screenId", "screen_id"))
		if sid == "" {
			continue
		}
		screenName := sourceText(sourceFirst(screen, "name", "screenName", "start_time_str"), 160)
		if screenName == "" {
			screenName = "场次 " + sid
		}
		tickets, ok := sourceFirst(screen, "ticket_list", "ticketList").([]any)
		if !ok {
			continue
		}
		flag, _, status := biliFlag(screen)
		screenOff := biliBlocked(screen, now, flag, status)
		for _, rawTicket := range tickets {
			ticket, ok := rawTicket.(map[string]any)
			if !ok {
				continue
			}
			sku := sourceID(sourceFirst(ticket, "id", "skuId", "sku_id"))
			if sku == "" {
				continue
			}
			key := [2]string{sid, sku}
			if _, exists := seen[key]; exists {
				return snapshot{}, &sourceError{message: "B站返回重复票档，未更新查票状态"}
			}
			seen[key] = struct{}{}
			num, hasNumber, detail := biliFlag(ticket)
			stock, hasStock := getInt(sourceFirst(ticket, "num", "stock", "stock_num", "stockNum"))
			canClick, _ := sourceBool(sourceFirst(ticket, "clickable", "canClick"))
			state := unknown
			switch {
			case screenOff || biliBlocked(ticket, now, num, detail) || (hasStock && stock == 0):
				state = unavailable
			case canClick && (num == 2 || num == 6 || (!hasNumber && biliOnLabel(detail))):
				state = available
			}
			label := sourceText(sourceFirst(ticket, "desc", "ticketDesc", "skuName", "name"), 160)
			if label == "" {
				label = "票档 " + sku
			}
			row := listing{screenID: sid, skuID: sku, screen: screenName, name: label, state: state, detail: detail}
			if price, ok := getInt(sourceFirst(ticket, "price", "ticketPrice", "ticket_price")); ok && price >= 0 {
				row.priceCents = &price
			}
			rows = append(rows, row)
		}
	}
	return snapshot{name: name, listings: rows}, nil
}

var (
	damaiSaleData    = regexp.MustCompile(`(?is)<div\b[^>]*\s+id\s*=\s*(?:"dataDefault"|'dataDefault')[^>]*>(.*?)</div\s*>`)
	damaiHTMLTitle   = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title\s*>`)
	damaiTitleSuffix = regexp.MustCompile(`\s*[【\[]网上订票[】\]]\s*[-－]\s*大麦网\s*$`)
)

func damaiOnLabel(code int64, button string) bool {
	switch code {
	case 204:
		switch button {
		case "立即购票", "立即购买", "特惠购票", "特价购票", "优惠购票", "立即预订":
			return true
		}
	case 206:
		return button == "选座购票" || button == "选座购买"
	}
	return false
}

// Inspect only the public project button. No scripts, inventory inference,
// session requests, or purchase actions are part of the Damai adapter.
func parseDamai(page string, e event, now time.Time) (snapshot, error) {
	matched := damaiSaleData.FindStringSubmatch(page)
	if matched == nil {
		lowerPage := strings.ToLower(page)
		for _, marker := range []string{"x5sec", "captcha", "人机验证", "访问验证"} {
			if strings.Contains(lowerPage, marker) {
				return snapshot{}, &sourceError{"大麦详情要求验证", sourceVerificationCooldown}
			}
		}
		return snapshot{}, &sourceError{message: "大麦公开详情页没有返回售卖数据"}
	}
	text := trimSourceSpace(matched[1])
	value, ok := decodeSourceJSON(text)
	if !ok {
		value, ok = decodeSourceJSON(html.UnescapeString(text))
		if !ok {
			return snapshot{}, &sourceError{message: "大麦公开售卖数据无法解析"}
		}
	}
	data, ok := value.(map[string]any)
	_, containsStatus := data["buyBtnStatus"]
	_, containsText := data["buyBtnText"]
	_, containsShow := data["buyBtnShow"]
	if !ok || !(containsStatus || containsText || containsShow) {
		return snapshot{}, &sourceError{message: "大麦公开售卖数据结构变化"}
	}
	name := ""
	if title := damaiHTMLTitle.FindStringSubmatch(page); title != nil {
		name = damaiTitleSuffix.ReplaceAllString(cleanText(html.UnescapeString(title[1]), 200), "")
	}
	if name == "" {
		name = "大麦活动 " + e.id
	}
	button := sourceText(data["buyBtnText"], 80)
	shown := sourceText(data["buyBtnShow"], 80)
	code, _ := getInt(data["buyBtnStatus"])
	saleStart, hasSaleStart := sourceSaleTime(data, "sellStartTime", "sellStartTimeStr")
	needsUpgrade, _ := sourceBool(data["appNeedUpGrade"])
	state := unknown
	switch {
	case sourceBlockedLabel(button) || sourceBlockedLabel(shown) || code == 91 || (hasSaleStart && now.Before(saleStart)):
		state = unavailable
	case !needsUpgrade && damaiOnLabel(code, shown) && (button == "" || damaiOnLabel(code, button)):
		state = available
	}
	if shown == "" {
		shown = button
	}
	return snapshot{name: name, listings: []listing{{name: "公开购票按钮", state: state, detail: shown}}}, nil
}

func (w *webClient) query(ctx context.Context, e event) (snapshot, error) {
	if !validID(e.id) {
		return snapshot{}, &sourceError{message: "活动编号无效"}
	}
	switch e.platform {
	case bili:
		reply, err := w.json(ctx, biliMall, map[string]any{"itemsId": json.Number(e.id), "itemsDetailPageType": 3})
		if err != nil {
			return snapshot{}, err
		}
		return parseBilibili(reply, e, time.Now())
	case damai:
		page, err := w.text(ctx, e.url(), nil, "text/html")
		if err != nil {
			return snapshot{}, err
		}
		return parseDamai(page, e, time.Now())
	default:
		return snapshot{}, &sourceError{message: "不支持的票务平台"}
	}
}
