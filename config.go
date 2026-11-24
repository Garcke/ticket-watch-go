package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	configFile      = "/etc/ticket-watch/config.json"
	credentialFile  = "/etc/ticket-watch/serverchan-key"
	credentialName  = "serverchan-key"
	stateFile       = "/var/lib/ticket-watch/state.sqlite3"
	defaultInterval = 60
	maxTargets      = 30
)

type target struct {
	event   event
	name    string
	screens []string
	skus    []string
}

func hasID(ids []string, id string) bool {
	if len(ids) == 0 {
		return true
	}
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func (t target) includes(l listing) bool {
	return hasID(t.screens, l.screenID) && hasID(t.skus, l.skuID)
}

func (t target) title(s snapshot) string {
	if t.name != "" {
		return t.name
	}
	return s.name
}

type config struct {
	interval int
	targets  []target
}

func (c config) period(p platform) time.Duration {
	s := c.interval
	if p == damai {
		s = max(s, 60)
	}
	return time.Duration(s) * time.Second
}

type rawConfig struct {
	Interval json.RawMessage `json:"interval_seconds"`
	Targets  json.RawMessage `json:"targets"`
}

type rawTarget struct {
	URL       string          `json:"url"`
	Name      json.RawMessage `json:"name"`
	ScreenIDs json.RawMessage `json:"screen_ids"`
	SKUIDs    json.RawMessage `json:"sku_ids"`
}

type outputTarget struct {
	URL       string   `json:"url"`
	Name      string   `json:"name,omitempty"`
	ScreenIDs []string `json:"screen_ids,omitempty"`
	SKUIDs    []string `json:"sku_ids,omitempty"`
}

func decodeStrict(data []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON input")
	}
	return nil
}

func readSmallFile(path string, maximum int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > maximum || !utf8.Valid(buf) {
		return nil, errors.New("invalid text file")
	}
	return buf, nil
}

func loadConfig(path string) (config, error) {
	const invalid = "不能读取配置，请检查文件权限和 JSON 格式"
	data, err := readSmallFile(path, 1<<20)
	if err != nil {
		return config{}, errors.New(invalid)
	}
	data = bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	var raw *rawConfig
	if decodeStrict(data, &raw) != nil || raw == nil {
		return config{}, errors.New(invalid)
	}
	c := config{interval: defaultInterval}
	if len(raw.Interval) != 0 {
		if json.Unmarshal(raw.Interval, &c.interval) != nil || bytes.Equal(raw.Interval, []byte("null")) || c.interval < 1 || c.interval > 3600 {
			return config{}, errors.New("interval_seconds 必须在 1–3600 秒之间")
		}
	}
	var entries []json.RawMessage
	if len(raw.Targets) != 0 {
		if json.Unmarshal(raw.Targets, &entries) != nil || entries == nil || len(entries) > maxTargets {
			return config{}, errors.New("targets 必须为数组，最多配置 30 个活动")
		}
	}
	seen := make(map[event]bool)
	for i, entry := range entries {
		t, err := decodeTarget(entry)
		if err != nil {
			return config{}, fmt.Errorf("目标 #%d：%w", i+1, err)
		}
		if seen[t.event] {
			return config{}, fmt.Errorf("目标 #%d：活动已重复配置", i+1)
		}
		seen[t.event] = true
		c.targets = append(c.targets, t)
	}
	return c, nil
}

func decodeTarget(data []byte) (target, error) {
	var raw *rawTarget
	if decodeStrict(data, &raw) != nil || raw == nil {
		return target{}, errors.New("目标仅支持 url、name、screen_ids 和 sku_ids")
	}
	var name string
	if len(raw.Name) > 0 && (json.Unmarshal(raw.Name, &name) != nil || bytes.Equal(raw.Name, []byte("null"))) {
		return target{}, errors.New("活动备注须为不超过 160 字的文本")
	}
	screens, err := decodeIDs(raw.ScreenIDs)
	if err != nil {
		return target{}, err
	}
	skus, err := decodeIDs(raw.SKUIDs)
	if err != nil {
		return target{}, err
	}
	return makeTarget(raw.URL, name, screens, skus)
}

func decodeIDs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var vals []json.RawMessage
	if json.Unmarshal(raw, &vals) != nil || vals == nil || len(vals) > 100 {
		return nil, errors.New("场次和票档编号必须为数组（最多 100 个）")
	}
	ids := make([]string, 0, len(vals))
	for _, v := range vals {
		if len(v) != 0 && v[0] == '"' {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, errors.New("场次和票档编号必须为正整数")
			}
			ids = append(ids, s)
		} else {
			ids = append(ids, string(v))
		}
	}
	return ids, nil
}

func normalizeIDs(ids []string) ([]string, error) {
	if len(ids) > 100 {
		return nil, errors.New("场次和票档编号必须为数组（最多 100 个）")
	}
	seen := make(map[string]bool)
	var out []string
	for _, id := range ids {
		if !validID(id) {
			return nil, errors.New("场次和票档编号必须为正整数")
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

func makeTarget(rawURL, name string, screens, skus []string) (target, error) {
	ev, err := parseEventURL(rawURL)
	if err != nil {
		return target{}, err
	}
	if utf8.RuneCountInString(name) > 160 {
		return target{}, errors.New("活动备注须为不超过 160 字的文本")
	}
	screens, err = normalizeIDs(screens)
	if err != nil {
		return target{}, err
	}
	skus, err = normalizeIDs(skus)
	if err != nil {
		return target{}, err
	}
	if ev.platform == damai && len(screens)+len(skus) > 0 {
		return target{}, errors.New("大麦只支持项目级购票按钮，不能指定场次或票档")
	}
	return target{ev, cleanText(name, 160), screens, skus}, nil
}

func saveConfig(path string, c config) error {
	if len(c.targets) > maxTargets {
		return errors.New("最多配置 30 个活动")
	}
	out := struct {
		Interval int            `json:"interval_seconds"`
		Targets  []outputTarget `json:"targets"`
	}{c.interval, make([]outputTarget, 0, len(c.targets))}
	for _, t := range c.targets {
		out.Targets = append(out.Targets, outputTarget{t.event.url(), t.name, t.screens, t.skus})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'), 0644)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	parent := filepath.Dir(path)
	f, err := os.CreateTemp(parent, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func takeLock(path string, nonblocking bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_EX
	if nonblocking {
		mode |= syscall.LOCK_NB
	}
	for {
		err = syscall.Flock(int(f.Fd()), mode)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func loadKey() (string, error) {
	path := credentialFile
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		path = filepath.Join(dir, credentialName)
	}
	data, err := readSmallFile(path, 512)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("无法读取 Server酱³ SendKey；管理命令请在服务器上用 root 运行")
	}
	key := strings.TrimSpace(string(data))
	if key != "" && !keyPattern.MatchString(key) {
		return "", errors.New("需要 sctp 开头的 Server酱³ SendKey，不能使用 Turbo Key")
	}
	return key, nil
}
