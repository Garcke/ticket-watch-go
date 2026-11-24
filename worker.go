package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

const (
	sourceGap     = 3 * time.Second
	biliFastGap   = 500 * time.Millisecond
	pushRetryBase = 120 * time.Second
)

type pollDeps struct {
	now   func() time.Time
	wait  func(context.Context, time.Duration) bool
	query func(context.Context, event) (snapshot, error)
	send  func(context.Context, string, string) error
}

func waitFor(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

func cooldownOf(err error) time.Duration {
	var source *sourceError
	if errors.As(err, &source) {
		return source.cooldown
	}
	return 0
}

func reasonOf(err error) string {
	var source *sourceError
	if errors.As(err, &source) {
		return source.message
	}
	return "请求未完成"
}

func nextRetry(period time.Duration, failures int, cooldown time.Duration) time.Duration {
	base := max(period, defaultInterval*time.Second)
	return max(cooldown, min(base*(1<<min(failures, 8)), max(30*time.Minute, base)))
}

type runTarget struct {
	target
	period, gap time.Duration
	fast        bool
	due         time.Time
	failures    int
	summary     [3]int
	summarySet  bool
}

func pickTarget(targets []runTarget, nextSource map[platform]time.Time) (int, time.Time) {
	index := 0
	at := maxTime(targets[0].due, nextSource[targets[0].event.platform])
	for i := 1; i < len(targets); i++ {
		t := &targets[i]
		when := maxTime(t.due, nextSource[t.event.platform])
		if when.Before(at) || (when.Equal(at) && t.due.Before(targets[index].due)) {
			index, at = i, when
		}
	}
	return index, at
}

func poll(ctx context.Context, c config, state *stateStore, deps pollDeps) error {
	if len(c.targets) == 0 {
		return nil
	}
	start := deps.now()
	nextSource := make(map[platform]time.Time)
	targets := make([]runTarget, 0, len(c.targets))
	for _, t := range c.targets {
		p := t.event.platform
		if _, seen := nextSource[p]; !seen {
			until, err := state.pausedUntil(ctx, string(p))
			if err != nil {
				return err
			}
			nextSource[p] = start.Add(max(0, until.Sub(start)))
			if until.After(start) {
				log.Printf("INFO %s 冷却中，预计 %s 后再查", p.label(), until.Sub(start).Round(time.Second))
			}
		}
		period, gap := c.period(p), sourceGap
		fast := p == bili && period < sourceGap
		if fast {
			gap = biliFastGap
		}
		targets = append(targets, runTarget{target: t, period: period, gap: gap, fast: fast, due: start})
	}
	until, err := state.pausedUntil(ctx, "serverchan")
	if err != nil {
		return err
	}
	nextSend := start.Add(max(0, until.Sub(start)))
	for ctx.Err() == nil {
		i, at := pickTarget(targets, nextSource)
		if !deps.wait(ctx, at.Sub(deps.now())) || ctx.Err() != nil {
			break
		}
		t := &targets[i]
		ev := t.event
		started := deps.now()
		snap, err := deps.query(ctx, ev)
		now := deps.now()
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			t.failures = min(t.failures+1, 8)
			cooldown := cooldownOf(err)
			delay := nextRetry(t.period, t.failures, cooldown)
			t.due = now.Add(delay)
			nextSource[ev.platform] = now.Add(max(sourceGap, cooldown))
			if cooldown > 0 {
				if err := state.pause(ctx, string(ev.platform), now.Add(cooldown)); err != nil {
					return err
				}
			}
			log.Printf("WARN %s 查询失败：%s；约 %s 后重试", ev.key(), reasonOf(err), delay.Round(time.Second))
			continue
		}
		dueBase := now
		if t.fast {
			// Count from request start in fast mode, with no pile-up or overlapping requests.
			dueBase = started
		}
		t.due, nextSource[ev.platform] = dueBase.Add(t.period), dueBase.Add(t.gap)
		t.failures = 0
		observed := make([]listing, 0, len(snap.listings))
		var summary [3]int
		for _, row := range snap.listings {
			if !t.includes(row) {
				continue
			}
			observed = append(observed, row)
			switch row.state {
			case available:
				summary[0]++
			case unavailable:
				summary[1]++
			default:
				summary[2]++
			}
		}
		if !t.summarySet || t.summary != summary {
			note := "未返回匹配的可监控状态，暂为未知"
			if len(observed) > 0 {
				note = fmt.Sprintf("可购 %d，不可购 %d，未知 %d", summary[0], summary[1], summary[2])
			}
			log.Printf("INFO %s %s | %s", ev.key(), t.title(snap), note)
			t.summary, t.summarySet = summary, true
		}
		pending, err := state.pending(ctx, ev, observed)
		if err != nil {
			return err
		}
		if len(pending) == 0 || now.Before(nextSend) || ctx.Err() != nil {
			continue
		}
		title, body := buildNotice(t.target, snap, pending, now)
		if err := deps.send(ctx, title, body); err != nil {
			if ctx.Err() != nil {
				break
			}
			delay := max(cooldownOf(err), pushRetryBase)
			nextSend = deps.now().Add(delay)
			if err := state.pause(ctx, "serverchan", nextSend); err != nil {
				return err
			}
			log.Printf("WARN %s 推送未确认：%s；等待 %s 后的后续可购状态再试", ev.key(), reasonOf(err), delay.Round(time.Second))
			continue
		}
		// Persist an already accepted push even if a stop signal arrives just then.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Second)
		err = state.markSent(saveCtx, ev, pending)
		cancel()
		if err != nil {
			return err
		}
		log.Printf("INFO %s Server酱³ 已接受提醒：%d 项", ev.key(), len(pending))
	}
	return nil
}
