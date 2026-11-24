package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

const version = "1.0.0-go"

func showHelp(out io.Writer) {
	fmt.Fprint(out, `B站会员购和大麦公开售卖状态监控（只查票，不下单）
用法：ticket-watch [--config 文件] <命令>
  run                      运行后台监控（由 systemd 调用）
  list                     列出活动和推送配置状态
  add <官方详情链接>       添加活动；可选 --name、--screen-id、--sku-id
  remove <官方详情链接>    从监控列表移除活动
  probe <官方详情链接>     查询一次，不推送、不修改监控状态
  set-key                  从交互式终端隐藏输入并保存 Server酱³ SendKey
  test-notify              发送一条 Server酱³ 测试通知
  check                    只读检查配置与 SQLite 状态，不访问网络
  version                  显示程序和 Go 版本
`)
}

func main() {
	syscall.Umask(0077)
	log.SetFlags(0)
	os.Exit(cli(os.Args[1:], os.Stdout, os.Stderr))
}

func cli(args []string, out, errOut io.Writer) int {
	fail := func(code int, err error) int {
		fmt.Fprintf(errOut, "ERROR %s\n", err)
		return code
	}
	globals := flag.NewFlagSet("ticket-watch", flag.ContinueOnError)
	globals.SetOutput(io.Discard)
	cfgPath := globals.String("config", configFile, "")
	showVersion := globals.Bool("version", false, "")
	if err := globals.Parse(args); errors.Is(err, flag.ErrHelp) {
		showHelp(out)
		return 0
	} else if err != nil {
		return fail(2, errors.New("命令格式错误，参见 ticket-watch --help"))
	}
	args = globals.Args()
	if *showVersion || (len(args) == 1 && args[0] == "version") {
		fmt.Fprintf(out, "ticket-watch %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return 0
	}
	if len(args) == 0 || args[0] == "help" {
		showHelp(out)
		return 0
	}
	command, remaining := args[0], args[1:]
	if len(remaining) == 1 && (remaining[0] == "--help" || remaining[0] == "-h") {
		showHelp(out)
		return 0
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var err error
	switch command {
	case "run", "check":
		flags := flag.NewFlagSet(command, flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		statePath := flags.String("state", stateFile, "")
		if flags.Parse(remaining) != nil || flags.NArg() != 0 {
			return fail(2, errors.New("命令格式错误，参见 ticket-watch --help"))
		}
		c, cfgErr := loadConfig(*cfgPath)
		if cfgErr != nil {
			return fail(78, cfgErr)
		}
		key, keyErr := loadKey()
		if keyErr != nil {
			return fail(78, keyErr)
		}
		if command == "check" {
			err = printCheck(ctx, out, c, key, *statePath)
		} else {
			if len(c.targets) == 0 || key == "" {
				log.Printf("INFO 服务待命：%d 个活动，SendKey %s。配置后请重启；待命时不轮询。", len(c.targets), keyStatus(key))
				<-ctx.Done()
				return 0
			}
			err = runService(ctx, c, key, *statePath)
			if errors.Is(err, errWorkerLocked) {
				return fail(78, err)
			}
		}
	case "list":
		if len(remaining) != 0 {
			return fail(2, errors.New("用法：ticket-watch list"))
		}
		var c config
		var key string
		if c, err = loadConfig(*cfgPath); err == nil {
			if key, err = loadKey(); err == nil {
				printList(out, c, key)
			}
		}
	case "set-key":
		if len(remaining) != 0 {
			return fail(2, errors.New("用法：ticket-watch set-key；不要将密钥作为参数"))
		}
		err = setKey(out, errOut)
	case "test-notify":
		if len(remaining) != 0 {
			return fail(2, errors.New("用法：ticket-watch test-notify"))
		}
		var key string
		if key, err = loadKey(); err == nil {
			err = sendNotice(ctx, newWebClient(), key, "票务监控测试", "Server酱³ API 测试。收到此消息说明推送通道可用。")
		}
		if err == nil {
			fmt.Fprintln(out, "Server酱³ 已接受测试通知，请打开手机 App 确认。")
		}
	case "probe":
		if len(remaining) != 1 {
			return fail(2, errors.New("用法：ticket-watch probe <官方详情链接>"))
		}
		err = printProbe(ctx, out, remaining[0])
	case "add", "remove":
		var t target
		if t, err = parseChange(command, remaining); err == nil {
			err = updateTargets(*cfgPath, command, t)
			if err == nil {
				verb := "添加"
				if command == "remove" {
					verb = "移除"
				}
				fmt.Fprintf(out, "已%s %s 活动 %s。运行 systemctl restart ticket-watch 后生效。\n", verb, t.event.platform.label(), t.event.id)
			}
		}
	default:
		return fail(2, errors.New("未知命令，参见 ticket-watch --help"))
	}
	if ctx.Err() != nil {
		if command == "run" {
			return 0
		}
		return 130
	}
	if err != nil {
		return fail(1, err)
	}
	return 0
}

var errWorkerLocked = errors.New("此状态目录已有监控进程在使用")

func runService(ctx context.Context, c config, key, path string) error {
	workerLock, err := takeLock(filepath.Join(filepath.Dir(path), "worker.lock"), true)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errWorkerLocked
	}
	if err != nil {
		return errors.New("无法打开监控进程锁")
	}
	defer workerLock.Close()
	state, err := openState(ctx, path, false)
	if err != nil {
		return errors.New("无法打开监控状态数据库")
	}
	defer state.close()
	cadence := make([]string, 0, 2)
	for _, p := range []platform{bili, damai} {
		for _, t := range c.targets {
			if t.event.platform == p {
				cadence = append(cadence, fmt.Sprintf("%s每活动目标间隔 %v", p.label(), c.period(p).Seconds())+" 秒")
				break
			}
		}
	}
	log.Printf("INFO 开始监控 %d 个活动（Go）；%s。", len(c.targets), strings.Join(cadence, "；"))
	client := newWebClient()
	deps := pollDeps{now: time.Now, wait: waitFor, query: client.query, send: func(ctx context.Context, title, body string) error {
		return sendNotice(ctx, client, key, title, body)
	}}
	if err := poll(ctx, c, state, deps); err != nil {
		return errors.New("监控状态数据库操作失败")
	}
	return nil
}

func keyStatus(key string) string {
	if key == "" {
		return "未配置"
	}
	return "已配置"
}

func printList(out io.Writer, c config, key string) {
	fmt.Fprintf(out, "轮询间隔 %d 秒；%d 个活动；SendKey %s\n", c.interval, len(c.targets), keyStatus(key))
	for _, t := range c.targets {
		label := t.event.platform.label()
		if seconds := int(c.period(t.event.platform) / time.Second); seconds != c.interval {
			label += fmt.Sprintf("（%d 秒）", seconds)
		}
		name := t.name
		if name == "" {
			name = t.event.id
		}
		fmt.Fprintf(out, "%s %s %s", label, name, t.event.url())
		if len(t.screens) > 0 {
			fmt.Fprintf(out, " 场次=%s", strings.Join(t.screens, ","))
		}
		if len(t.skus) > 0 {
			fmt.Fprintf(out, " 票档=%s", strings.Join(t.skus, ","))
		}
		fmt.Fprintln(out)
	}
}

func printCheck(ctx context.Context, out io.Writer, c config, key, path string) error {
	printList(out, c, key)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(out, "SQLite 状态尚未初始化；未写入任何状态。")
		return nil
	}
	s, err := openState(ctx, path, true)
	if err != nil {
		return errors.New("无法只读打开监控状态数据库")
	}
	defer s.close()
	n, p, err := s.check(ctx, time.Now())
	if err != nil {
		return errors.New("监控状态数据库检查未通过")
	}
	fmt.Fprintf(out, "SQLite 检查通过；去重记录 %d 条；当前冷却 %d 项。未向外部发送请求。\n", n, p)
	return nil
}

func printProbe(ctx context.Context, out io.Writer, raw string) error {
	ev, err := parseEventURL(raw)
	if err != nil {
		return err
	}
	snap, err := newWebClient().query(ctx, ev)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s｜%s\n%s\n", ev.platform.label(), snap.name, ev.url())
	if len(snap.listings) == 0 {
		fmt.Fprintln(out, "没有返回可监控的场次和票档，状态未知。")
	}
	for _, row := range snap.listings {
		detail := ""
		if row.detail != "" {
			detail = "（" + row.detail + "）"
		}
		if row.screenID != "" {
			price := priceLabel(row.priceCents)
			if price != "" {
				price = " " + price
			}
			fmt.Fprintf(out, "[%s] 场次 %s %s ｜票档 %s %s%s%s\n", row.state, row.screenID, row.screen, row.skuID, row.name, price, detail)
		} else {
			fmt.Fprintf(out, "[%s] %s%s｜不代表具体场次或票档的库存\n", row.state, row.name, detail)
		}
	}
	return nil
}

func setKey(out, errOut io.Writer) error {
	fd := int(os.Stdin.Fd())
	if os.Geteuid() != 0 || !term.IsTerminal(fd) {
		return errors.New("请在服务器的交互式 root 终端运行 set-key")
	}
	fmt.Fprint(errOut, "请输入 Server酱³ SendKey（输入不回显）：")
	raw, err := term.ReadPassword(fd)
	fmt.Fprintln(errOut)
	if err != nil {
		return errors.New("未能从终端读取 SendKey")
	}
	defer clear(raw)
	key := strings.TrimSpace(string(raw))
	if !keyPattern.MatchString(key) {
		return errors.New("需要 sctp 开头的 Server酱³ SendKey，不能使用 Turbo Key")
	}
	if err := writeAtomic(credentialFile, []byte(key+"\n"), 0600); err != nil {
		return errors.New("无法保存 Server酱³ SendKey")
	}
	fmt.Fprintln(out, "SendKey 已保存。可用 ticket-watch test-notify 发送一条测试通知。")
	fmt.Fprintln(out, "运行 systemctl restart ticket-watch 载入新配置。")
	return nil
}

func parseChange(command string, args []string) (target, error) {
	var url, name string
	var screens, skus []string
	var positionalOnly bool
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" && !positionalOnly {
			positionalOnly = true
			continue
		}
		if strings.HasPrefix(arg, "-") && !positionalOnly {
			opt, val, hasValue := strings.Cut(arg, "=")
			if command != "add" || (opt != "--name" && opt != "--screen-id" && opt != "--sku-id") {
				return target{}, errors.New("不受支持的选项，参见 ticket-watch --help")
			}
			if !hasValue {
				i++
				if i == len(args) || strings.HasPrefix(args[i], "--") {
					return target{}, errors.New("选项缺少参数")
				}
				val = args[i]
			}
			switch opt {
			case "--name":
				name = val
			case "--screen-id":
				screens = append(screens, val)
			case "--sku-id":
				skus = append(skus, val)
			}
			continue
		}
		if url != "" || arg == "" {
			return target{}, errors.New("请填写一个官方活动详情链接")
		}
		url = arg
	}
	return makeTarget(url, name, screens, skus)
}

func updateTargets(path, command string, t target) error {
	l, err := takeLock(filepath.Join(filepath.Dir(path), ".config.lock"), false)
	if err != nil {
		return errors.New("无法锁定票务配置")
	}
	defer l.Close()
	c, err := loadConfig(path)
	if err != nil {
		return err
	}
	index := -1
	for i, row := range c.targets {
		if row.event == t.event {
			index = i
			break
		}
	}
	if command == "add" {
		if index >= 0 {
			return errors.New("活动已经存在。如需改变筛选条件，可编辑配置或先 remove 再 add。")
		}
		if len(c.targets) == maxTargets {
			return errors.New("最多配置 30 个活动")
		}
		c.targets = append(c.targets, t)
	} else {
		if index < 0 {
			return errors.New("找不到此监控目标")
		}
		c.targets = append(c.targets[:index], c.targets[index+1:]...)
	}
	if err := saveConfig(path, c); err != nil {
		return errors.New("配置保存失败")
	}
	return nil
}
