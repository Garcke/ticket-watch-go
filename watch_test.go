package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	bev1 = event{bili, "101"}
	bev2 = event{bili, "102"}
	dev1 = event{damai, "90002"}
)

const demoKey = "sctp123tunit-test-only"

func TestConfigCommands(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("{}")
	c, err := loadConfig(path)
	if err != nil || c.interval != 60 || len(c.targets) != 0 {
		t.Fatalf("unexpected default: %+v / %v", c, err)
	}
	write(fmt.Sprintf(`{"interval_seconds": 1, "targets": [{"url": %q, "screen_ids": [401,"401"]}]}`, bev1.url()))
	c, err = loadConfig(path)
	if err != nil || len(c.targets) != 1 || !reflect.DeepEqual(c.targets[0].screens, []string{"401"}) || c.period(bili) != time.Second || c.period(damai) != time.Minute {
		t.Fatalf("unexpected fast config: %+v / %v", c, err)
	}
	next, err := parseChange("add", []string{bev2.url(), "--sku-id", "901", "--sku-id=902", "--name", " 测试  活动 "})
	if err != nil || next.event != bev2 || next.name != "测试 活动" || len(next.skus) != 2 {
		t.Fatalf("add args: %+v / %v", next, err)
	}
	if err := updateTargets(path, "add", next); err != nil {
		t.Fatal(err)
	}
	if err := updateTargets(path, "add", next); err == nil {
		t.Fatal("accepted duplicate add")
	}
	if err := updateTargets(path, "remove", c.targets[0]); err != nil {
		t.Fatal(err)
	}
	c, err = loadConfig(path)
	if err != nil || c.interval != 1 || !reflect.DeepEqual(c.targets, []target{next}) {
		t.Fatalf("config after removal: %+v / %v", c, err)
	}
	for _, input := range []string{
		`null`, `{"targets":null}`, `{"interval_seconds":null}`, `{"interval_seconds":true}`,
		`{"interval_seconds":0}`, `{"interval_seconds":3601}`, `{"interval_seconds":1.5}`, `{"other":1}`, `{} {}`,
		fmt.Sprintf(`{"targets":[{"url":%q,"sku_ids":[5]}]}`, dev1.url()),
		fmt.Sprintf(`{"targets":[{"url":%q,"screen_ids":[1.2]}]}`, bev1.url()),
		fmt.Sprintf(`{"targets":[{"url":%q},{"url":%q}]}`, bev1.url(), bev1.url()),
	} {
		write(input)
		if _, err := loadConfig(path); err == nil {
			t.Errorf("accepted invalid config %s", input)
		}
	}
}

func TestHandoverKeepsSQLiteContract(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1795321254, 250000000)
	path := filepath.Join(t.TempDir(), "state.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// These statements populate the two tables and column types used by Python.
	if _, err := db.Exec(`CREATE TABLE notified (event TEXT NOT NULL, screen TEXT NOT NULL, sku TEXT NOT NULL, PRIMARY KEY (event, screen, sku));
	                      CREATE TABLE pause (service TEXT NOT NULL PRIMARY KEY, until_time INTEGER NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO notified VALUES (?, ?, ?)`, bev1.key(), "1", "2"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO pause VALUES (?, ?)`, "bilibili", now.Unix()+180); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	check, err := openState(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}
	n, p, err := check.check(ctx, now)
	if err != nil || n != 1 || p != 1 {
		t.Fatalf("preflight: got %d, %d / %v", n, p, err)
	}
	check.close()

	s, err := openState(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.close() }()
	a := listing{screenID: "1", skuID: "2", state: available}
	b := listing{screenID: "1", skuID: "3", state: available}
	u := a
	u.state = unknown
	off := a
	off.state = unavailable
	want := func(rows, expected []listing) {
		t.Helper()
		got, err := s.pending(ctx, bev1, rows)
		if err != nil || len(got) != len(expected) {
			t.Fatalf("pending: got %+v / %v, want %+v", got, err, expected)
		}
		for i := range expected {
			if got[i] != expected[i] {
				t.Fatalf("pending: got %+v, want %+v", got, expected)
			}
		}
	}
	want([]listing{a, b}, []listing{b})
	want([]listing{u}, nil)
	want(nil, nil)
	want([]listing{a}, nil)
	want([]listing{off, b}, []listing{b})
	want([]listing{a, b}, []listing{a, b})
	if err := s.markSent(ctx, bev1, []listing{a, b}); err != nil {
		t.Fatal(err)
	}
	// A shorter cooldown must never replace a previously noticed longer one.
	if err := s.pause(ctx, "bilibili", now.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	s, err = openState(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	want([]listing{a, b}, nil)
	until, err := s.pausedUntil(ctx, "bilibili")
	if err != nil || until.Unix() != now.Unix()+180 {
		t.Fatalf("persisted cooldown: %s / %v", until, err)
	}
}

type stubPoster struct {
	url     string
	payload map[string]string
	result  map[string]any
	err     error
}

func (p *stubPoster) json(_ context.Context, url string, body any) (map[string]any, error) {
	p.url, p.payload = url, body.(map[string]string)
	return p.result, p.err
}

func TestServerChanAndMessages(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 23, 8, 30, 5, 0, time.UTC)
	p := &stubPoster{result: map[string]any{"code": json.Number("0")}}
	n := int64(19800)
	row := listing{screen: "11月28日", name: "普通[入场券]", state: available, priceCents: &n}
	title, body := buildNotice(target{event: bev1}, snapshot{name: "FES"}, []listing{row}, now)
	if !strings.Contains(body, `普通\[入场券\] · ¥198.00`) || !strings.Contains(body, "16:30:05") || !strings.Contains(body, bev1.url()) {
		t.Fatal(body)
	}
	if err := sendNotice(ctx, p, demoKey, title, body); err != nil {
		t.Fatal(err)
	}
	if p.url != "https://123.push.ft07.com/send/"+demoKey+".send" || p.payload["title"] != title || p.payload["desp"] != body || p.payload["tags"] != "票务监控" {
		t.Fatal("unexpected Server酱³ request")
	}
	p.result = map[string]any{"code": 123, "message": demoKey}
	if err := sendNotice(ctx, p, demoKey, title, body); err == nil || strings.Contains(err.Error(), demoKey) || cooldownOf(err) != 120*time.Second {
		t.Fatalf("nonzero response: %v", err)
	}
	p.err = errors.New("unexpected transport error: " + demoKey)
	if err := sendNotice(ctx, p, demoKey, title, body); err == nil || strings.Contains(err.Error(), demoKey) {
		t.Fatalf("transport error: %v", err)
	}
	if err := sendNotice(ctx, p, "SCT-turbo", title, body); err == nil {
		t.Fatal("accepted Turbo key")
	}
	_, body = buildNotice(target{event: dev1}, snapshot{name: "音乐会"}, []listing{{detail: "立即购票"}}, now)
	if !strings.Contains(body, "不代表某一场次或票档有余票") || !strings.Contains(body, dev1.url()) {
		t.Fatal(body)
	}
}

func newTestState(t *testing.T) *stateStore {
	t.Helper()
	s, err := openState(context.Background(), filepath.Join(t.TempDir(), "state.sqlite3"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.close() })
	return s
}

type pollStep struct {
	id string
	at time.Duration
}

func TestSchedulerFastCadenceAndPlatformCooldown(t *testing.T) {
	clockStart := time.Unix(1795321254, 0)
	c := config{interval: 1, targets: []target{{event: bev1}, {event: bev2}, {event: dev1}}}
	for _, limited := range []bool{false, true} {
		s := newTestState(t)
		now := clockStart
		var calls []pollStep
		deps := pollDeps{now: func() time.Time { return now }, wait: func(_ context.Context, d time.Duration) bool {
			now = now.Add(max(0, d))
			return now.Sub(clockStart) < 2800*time.Millisecond
		}, query: func(_ context.Context, ev event) (snapshot, error) {
			calls = append(calls, pollStep{ev.id, now.Sub(clockStart)})
			now = now.Add(125 * time.Millisecond)
			if limited && ev.platform == bili && len(calls) == 1 {
				return snapshot{}, &sourceError{message: "HTTP 429", cooldown: 180 * time.Second}
			}
			return snapshot{name: "示例"}, nil
		}, send: func(_ context.Context, _, _ string) error {
			t.Fatal("empty snapshot triggered a push")
			return nil
		}}
		ctx := context.Background()
		if err := poll(ctx, c, s, deps); err != nil {
			t.Fatal(err)
		}
		expected := []pollStep{{bev1.id, 0}, {dev1.id, 125 * time.Millisecond}}
		if !limited {
			expected = append(expected, pollStep{bev2.id, biliFastGap}, pollStep{bev1.id, time.Second}, pollStep{bev2.id, 1500 * time.Millisecond}, pollStep{bev1.id, 2 * time.Second}, pollStep{bev2.id, 2500 * time.Millisecond})
		}
		if !reflect.DeepEqual(calls, expected) {
			t.Fatalf("limited=%t: got %v, want %v", limited, calls, expected)
		}
		if limited {
			until, err := s.pausedUntil(ctx, "bilibili")
			if err != nil || until.Sub(clockStart) != 181*time.Second {
				t.Fatalf("persisted pause: %s / %v", until, err)
			}
			now = clockStart.Add(90 * time.Second)
			calls = nil
			deps.wait = func(_ context.Context, d time.Duration) bool {
				now = now.Add(max(0, d))
				return now.Sub(clockStart) < 91*time.Second
			}
			if err := poll(ctx, c, s, deps); err != nil || !reflect.DeepEqual(calls, []pollStep{{dev1.id, 90 * time.Second}}) {
				t.Fatalf("restart queried paused source: %v / %v", calls, err)
			}
		}
	}
}

func TestPollRetryDedupAndRestock(t *testing.T) {
	start := time.Unix(1795321254, 0)
	now := start
	s := newTestState(t)
	c := config{interval: 1, targets: []target{{event: bev1}}}
	var alerts []time.Duration
	deps := pollDeps{now: func() time.Time { return now }, wait: func(_ context.Context, d time.Duration) bool {
		now = now.Add(max(0, d))
		return now.Sub(start) <= 125*time.Second
	}, query: func(_ context.Context, _ event) (snapshot, error) {
		st := available
		switch now.Sub(start) {
		case 121 * time.Second:
			st = unknown
		case 123 * time.Second:
			st = unavailable
		}
		return snapshot{name: "示例", listings: []listing{{screenID: "1", skuID: "2", state: st}}}, nil
	}, send: func(_ context.Context, _, _ string) error {
		alerts = append(alerts, now.Sub(start))
		if len(alerts) == 1 {
			return errors.New("synthetic send failure")
		}
		return nil
	}}
	if err := poll(context.Background(), c, s, deps); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(alerts, []time.Duration{0, 120 * time.Second, 124 * time.Second}) {
		t.Fatalf("unexpected notifications: %v", alerts)
	}
}
