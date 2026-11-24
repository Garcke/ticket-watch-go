# ticket-watch（Go）

运行在 Linux 上的 B 站会员购／大麦票务监控，有票时通过 [Server酱³](https://sc3.ft07.com/) 推送。只查询公开详情，不登录票务账号，不自动购票或付款，不开放入站端口。

- **B 站会员购**：可按活动、场次或票档监控。只有明确可购且未被库存或销售状态阻断的票档才提醒。
- **大麦**：只检查公开网页的项目级购票入口；入口可用不代表具体场次或票档有库存。候补、缺货登记和未知状态不提醒。
- 同一票档持续显示可购只提醒一次；明确转为不可购后再次放票会重新提醒。去重和平台冷却保存在 SQLite 中，重启不丢失。

## 构建与测试

需要 Linux 和 Go 1.26 或更新版本。SQLite 驱动使用纯 Go 实现，关闭 CGO 即可构建，无需额外安装 SQLite。测试使用合成响应，不会向票务平台或真实 Server酱³ 接收端发请求。

```bash
CGO_ENABLED=0 go test -mod=readonly ./...
CGO_ENABLED=0 go vet -mod=readonly ./...
mkdir -p bin
CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -o bin/ticket-watch .
./bin/ticket-watch version
```

## 在 Linux 服务器部署

下列命令用于全新部署；若已安装，请先备份现有程序和配置，不要覆盖凭据或数据库。需要支持 `LoadCredential=` 的 systemd。

```bash
sudo install -o root -g root -m 0755 bin/ticket-watch /usr/local/bin/ticket-watch
sudo install -d -o root -g root -m 0755 /etc/ticket-watch
if ! sudo test -e /etc/ticket-watch/config.json; then
  sudo install -o root -g root -m 0644 config.example.json /etc/ticket-watch/config.json
fi
sudo ticket-watch set-key
sudo ticket-watch add '<替换为官方活动详情链接>'
sudo install -o root -g root -m 0644 deploy/ticket-watch.service /etc/systemd/system/ticket-watch.service
sudo systemctl daemon-reload
sudo systemctl enable --now ticket-watch.service
sudo ticket-watch check
```

`set-key` 从终端隐藏输入并保存 Server酱³ SendKey，不要把它放进命令参数、代码或仓库。若想确认手机能收到推送，可单独运行 `sudo ticket-watch test-notify` 发送一条测试通知。

## 日常维护

```bash
sudo ticket-watch list
sudo ticket-watch check                         # 只读检查配置和状态，不访问网络
ticket-watch probe '<官方活动详情链接>'            # 查一次，不推送
sudo ticket-watch add '<B站官方活动详情链接>' --screen-id 场次ID --sku-id 票档ID
sudo ticket-watch remove '<官方活动详情链接>'
sudo systemctl restart ticket-watch.service
sudo journalctl -u ticket-watch.service -n 30 --no-pager
```

- 场次和票档筛选可省略，可多次指定。短链请先在浏览器展开成官方详情链接。修改配置或密钥后重启服务。
- 配置在 `/etc/ticket-watch/config.json`。`interval_seconds` 默认为 60 秒；B 站支持 1–3600 秒，大麦至少 60 秒；最多配置 30 个活动。快速模式下同平台顺序查询，不并发；慢响应、平台限流或网络异常会拉长实际间隔。不会绕过验证或限流，售卖情况以官方页面为准。
- Server酱³ 密钥在 `/etc/ticket-watch/serverchan-key`（权限 `0600`），由 systemd 注入。去重和冷却记录在 `/var/lib/ticket-watch/state.sqlite3`；此前 Python 版使用的同名表可以沿用。仓库不包含生产配置、密钥或数据库。

## 参考

- [TaiMiao/Bili_Ticket_Monitor](https://github.com/TaiMiao/Bili_Ticket_Monitor)：B 站票档状态监测。
- [WittF/fnmdp](https://github.com/WittF/fnmdp)：原 Python 版的 B 站商城商详与票档状态参考。
- [shiyutim/tickets](https://github.com/shiyutim/tickets)：双平台余票监测思路；不包含其购票功能。
- [Server酱³ API 文档](https://doc.sc3.ft07.com/zh/serverchan3/server/api)。
