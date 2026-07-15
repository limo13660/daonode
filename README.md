# daonode

daonode 是为 [DaoBoard](https://github.com/limo13660/DaoBoard) 提供服务的独立节点后端。目前只集成 [Mieru](https://github.com/enfein/mieru)，并直接使用 Mieru 官方 Go Server API，不包含 V2Ray、Xray、Hysteria 等其他协议运行时。

当前代码对齐 Mieru `v3.34.1`。节点配置、用户同步、流量统计、路由组和订阅下发由 DaoBoard 管理，Mieru 连接与加密传输由官方运行时处理。

## 功能

- Mieru TCP、UDP 和混合端口绑定
- 官方 SOCKS5 代理请求，支持 TCP CONNECT 和 UDP ASSOCIATE
- IPv4、IPv6、多用户和动态用户同步
- 单端口、多端口和连续端口范围
- 官方 MTU、Multiplexing、Handshake Mode
- 客户端与服务端独立 Traffic Pattern
- Mieru User Hint（可选强制校验，默认兼容旧客户端）
- 用户动态增删、限速、在线状态和流量上报
- 用户到期、续费、流量重置和套餐限制变更使用单一监控任务，并按面板配置的轮询周期同步
- DaoBoard 路由组、DNS 规则、域名/IP/端口/协议阻断
- 兼容 v2node 的 `geoip.dat`、`geosite.dat` 路由数据格式
- 配置热更新和安全退出
- Linux TCP BBR 与 UDP socket 缓冲优化
- 仅发布 Linux 构建（amd64、386、arm/arm64、riscv64、MIPS、PPC64、s390x）

Mieru 本身不使用 TLS 证书。daonode 保留了节点框架的证书管理能力，供以后接入需要 TLS 的其他协议使用。

## 工作流程

```text
客户端（Mieru / Shadowrocket / 支持 Mieru 的内核）
                         |
                         v
                    daonode
                         |
             Mieru 官方 Server API
                         |
                         v
                    目标网站

DaoBoard <-> 节点配置、用户、路由、流量统计 <-> daonode
```

## Mieru 内核文件

daonode 不复制 Mieru 源码，而是通过 Go module 直接链接官方 `github.com/enfein/mieru/v3`。当前锁定版本为 `v3.34.1`；Mieru 源码位于本机 Go module cache，不在本仓库内。

| 文件 | 内核适配职责 |
|---|---|
| `go.mod`、`go.sum` | 锁定 Mieru 版本和校验值 |
| `core/core.go` | 根据面板下发的 `kernel` 与 `protocol` 校验能力并创建对应内核运行时 |
| `core/contract/runtime.go` | 所有内核统一实现的生命周期、用户同步和流量统计接口 |
| `core/shared/runtime.go` | 所有内核共用的累计流量差量、上报提交、限速、设备限制、在线连接跟踪和删用户连接清理 |
| `core/mieru/runtime.go` | Mieru Server API、TCP/UDP 监听、端口绑定、认证用户同步、Traffic Pattern、User Hint、SOCKS5 TCP/UDP 转发和原始累计计数读取 |
| `api/v2board/node.go` | 将 DaoBoard 节点配置解析为 Mieru 通用配置；校验传输协议、端口、端口范围和 User Hint 字段 |
| `node/controller.go` | 节点生命周期、配置热更新、拉取用户、上报在线状态和流量 |
| `node/node.go`、`node/user.go` | 节点运行循环和用户增删同步 |
| `core/mieru/route.go` | Mieru 请求的 TCP、UDP 路由、DNS 解析和规则匹配 |
| `core/mieru/geodata.go` | `geoip.dat`、`geosite.dat` 读取及 `geoip:private` 内置规则 |
| `.github/workflows/release.yml` | Linux 构建、打包 GeoIP/GeoSite 和发布包 |
| `script/install.sh` | 安装二进制、配置、路由数据和 systemd 服务 |

主要使用的官方 Mieru 包包括：

```text
github.com/enfein/mieru/v3/apis/server
github.com/enfein/mieru/v3/apis/model
github.com/enfein/mieru/v3/apis/trafficpattern
github.com/enfein/mieru/v3/pkg/appctl/appctlpb
github.com/enfein/mieru/v3/pkg/sockopts
github.com/enfein/mieru/v3/pkg/metrics
```

## 升级 Mieru 内核

升级只应从官方 Mieru tag 开始，不要直接替换 module cache 中的文件。建议流程如下：

1. 阅读新版 Mieru 的 release notes、`server-install.zh_CN.md`、`client-install.zh_CN.md`、`protocol.zh_CN.md` 和 `traffic-pattern.zh_CN.md`，记录配置字段、枚举值、User Hint、TCP/UDP 行为变化。
2. 在项目根目录更新依赖并整理 module：

   ```bash
   go get github.com/enfein/mieru/v3@vX.Y.Z
   go mod tidy
   ```

3. 优先检查 `core/mieru/runtime.go` 和 `api/v2board/node.go` 的编译错误及 API 变化，重点确认 `ServerConfig`、`AdvancedSettings`、`PortBinding`、`TransportProtocol`、Traffic Pattern 和 Packet Listener 接口。
4. 运行完整测试和 Linux 构建：

   ```bash
   GOEXPERIMENT=jsonv2 go test ./... -count=1
   GOEXPERIMENT=jsonv2 go build -trimpath -o daonode .
   ```

5. 至少验证 TCP、UDP、混合端口、端口范围、多用户、Traffic Pattern、User Hint 兼容模式、路由组、IPv4/IPv6 和官方 `mierus://` 分享链接。
6. 更新 README 中的 Mieru 版本和上游文档链接，再检查 `.github/workflows/release.yml` 的构建产物和 `script/install.sh` 的安装路径。
7. 先在测试节点滚动升级，确认 `journalctl -u daonode`、TCP/UDP 连接和流量上报正常后，再替换生产二进制。升级失败时保留上一版二进制和 `go.mod/go.sum` 进行回滚。

不要直接修改 `core/mieru/runtime.go` 来复制官方协议实现；协议、加密、重放检测、拥塞控制和 Traffic Pattern 应继续由官方 Mieru API 处理，daonode 只维护面板字段映射、生命周期和路由集成。

## 多内核约定

面板配置必须同时下发 `protocol` 和 `kernel`。`core/core.go` 会先检查所选内核是否明确支持该协议，再创建运行时；未下发 `kernel`、内核不存在或协议不受支持时，节点会拒绝启动或重载，不再为旧配置自动选择 Mieru。

每个新内核必须放在独立的 `core/<kernel>/` 目录，实现 `core/contract.Runtime`，并在根内核能力表中登记其支持的协议。不要只增加面板下拉选项；面板能力表、保存校验、配置下发、后端适配和运行验证必须一起完成。

新内核应匿名嵌入 `core/shared.RuntimeServices`，只实现协议相关的启动停止、认证用户同步、连接处理和按 UID 读取原始累计流量。认证成功后统一调用 `OpenConnection`，用户事务成功后统一调用 `SyncUsers`；流量差量、失败重试、提交确认、计数器重置、限速、设备数和连接释放不应在各内核中重复实现。

## 安装

建议在 DaoBoard 后台复制对应节点的“一键安装命令”。命令会自动写入面板地址、节点 ID 和通讯密钥。

也可以手动运行安装脚本：

```bash
wget -N https://raw.githubusercontent.com/limo13660/daonode/main/script/install.sh
bash install.sh
```

安装目录和服务文件：

```text
/usr/local/daonode/daonode
/etc/daonode/config.json
/etc/daonode/geoip.dat
/etc/daonode/geosite.dat
/etc/systemd/system/daonode.service
/etc/sysctl.d/99-daonode-network.conf
```

常用命令：

```bash
systemctl status daonode
systemctl restart daonode
journalctl -u daonode -n 100 --no-pager
```

## 配置文件

默认配置文件是 `/etc/daonode/config.json`：

```json
{
  "Log": {
    "Level": "warning",
    "Output": "",
    "Access": "none"
  },
  "Nodes": [
    {
      "ApiHost": "https://panel.example.com/",
      "NodeID": 1,
      "ApiKey": "change-me",
      "Timeout": 15,
      "RetryCount": 1
    }
  ]
}
```

一个 daonode 进程可以配置多个 DaoBoard 节点。生产环境建议使用 `warning` 或 `info` 日志级别；排查连接问题时再临时使用 `debug`。

## DaoBoard 节点字段

| 字段 | 说明 |
|---|---|
| 连接端口 | 订阅下发给客户端的端口 |
| 监听端口 | daonode 实际监听的端口，适用于 NAT/端口映射 |
| 传输协议 | 官方 `TCP` 或 `UDP` |
| 附加端口绑定 | 官方多端口、端口范围和混合 TCP/UDP 配置，默认值为 `[]` |
| MTU | 官方范围 `1280-1400`，客户端和服务端必须一致 |
| 多路复用 | `OFF`、`LOW`、`MIDDLE`、`HIGH` |
| 握手模式 | `STANDARD` 或 `NO_WAIT` |
| 客户端流量特征 | `mieru export traffic-pattern` 输出的 Base64 protobuf；留空保存时自动生成独立的随机 16 位官方 Base64 值 |
| 服务端流量特征 | `mita export traffic-pattern` 输出的 Base64 protobuf；留空保存时自动生成独立的随机 16 位官方 Base64 值 |
| 强制用户提示 | 使用客户端发送的 User Hint 加速多用户解密；默认关闭，开启后不支持 User Hint 的客户端会被拒绝 |

面板和后端支持 `1-65535` 端口。Mieru 官方建议使用 `1025-65535`；监听 `1-1024` 需要 root 或对应的 Linux capability。

附加端口绑定示例：

```json
[
  {
    "port": "8443",
    "server_port": "9443",
    "protocol": "TCP"
  },
  {
    "port": "10000-10010",
    "server_port": "11000-11010",
    "protocol": "UDP"
  }
]
```

`port` 是客户端连接端口，`server_port` 是 daonode 监听端口。没有 NAT 映射时两者应保持一致。

监听地址由面板的 `listen_ip` 控制：

- `0.0.0.0`：只明确监听 IPv4。
- `::`：监听 IPv6；当 Linux `net.ipv6.bindv6only=0` 时同时接受 IPv4。
- 指定 IPv4/IPv6：只监听该网卡地址。

IPv6 节点建议填写 `::`，并确认防火墙、云安全组和 Docker/宿主机网络已经放行对应 TCP 或 UDP 端口。若需要一个 IPv6 socket 同时接收 IPv4，检查 `sysctl net.ipv6.bindv6only` 的结果为 `0`。

附加绑定是“主端口之外的绑定列表”，不是替代主端口。每个对象的 `port` 与 `protocol` 在客户端配置中按相同位置配对；使用端口范围时，客户端和服务端范围长度必须相同。没有额外端口时直接保留默认值 `[]` 即可。

## User Hint

从 Mieru v3.31.0 开始，官方客户端会在加密握手中携带 User Hint。服务端可以先用这个提示定位用户名，再进行完整解密，用户较多时能显著减少 CPU 消耗。`强制用户提示` 对应官方 `advancedSettings.userHintIsMandatory`：

- 关闭：兼容旧客户端，但没有 User Hint 的请求会增加服务端解密尝试。
- 开启：只接受支持 User Hint 的 Mieru 客户端；旧客户端会连接失败。

Shadowrocket/iOS 等客户端如果尚未支持 User Hint，应保持关闭；支持 User Hint 的客户端在关闭强制校验时仍会自动携带并使用 User Hint。

建议确认所有客户端已升级到支持 User Hint 的版本后，再在专用节点上开启强制校验。

## 官方性能建议

### 1. 优先测试 TCP

Mieru 官方说明 TCP 通常比 UDP 更快，因为 UDP 数据包需要更多解密尝试。普通网络建议先使用 TCP；只有 TCP 在高峰期被 QoS、丢包或限速时，再测试 UDP。

### 2. TCP 使用 BBR

官方建议 Linux TCP 启用 BBR。安装脚本默认加载 `tcp_bbr`、写入开机模块配置，并设置：

```text
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
```

安装完成后检查结果：

```bash
sysctl net.ipv4.tcp_congestion_control
sysctl net.core.default_qdisc
```

Mieru UDP 协议内部已经使用 BBR，不受 Linux `tcp_congestion_control` 设置影响。

### 3. 保证 MTU 一致

客户端与服务端 MTU 必须一致。先使用官方默认值 `1400`；出现丢包、卡顿或路径分片时，两端一起改成 `1280` 再测试。

Linux 可以检查路径 MTU：

```bash
ping -M do -s 1372 <服务器地址>
```

### 4. 多路复用并非越高越快

推荐从 `MULTIPLEXING_LOW` 开始。大量短连接可以测试 `MIDDLE`；官方文档指出过于激进的多路复用可能导致连接数过高和速度下降，因此不要把 `HIGH` 当作通用加速选项。

### 5. 0-RTT 只降低建连延迟

`HANDSHAKE_NO_WAIT` 可以减少新连接等待时间，适合高延迟线路，但不会直接提高大文件吞吐量。稳定性优先时使用 `HANDSHAKE_STANDARD`。

### 6. 流量特征用于抗识别

Traffic Pattern 用于改变 TCP 分片、Nonce 和 Padding 行为，主要作用是降低 DPI 和流量分析识别。TCP 分片、较大的 Padding，以及对每个 UDP 包应用 Nonce Pattern 都可能增加延迟或带宽开销。没有明显 QoS/封锁时建议留空。

### 7. UDP 系统缓冲

安装脚本会设置 32 MiB socket 缓冲上限、1 MiB 默认缓冲和更大的网络设备 backlog。daonode 同时为 Mieru UDP listener 和目标 UDP socket 请求 16 MiB 读写缓冲，并复用数据包内存与目标地址缓存。

重新应用设置：

```bash
sysctl -p /etc/sysctl.d/99-daonode-network.conf
systemctl restart daonode
```

## 速度排查

```bash
# 查看协议监听
ss -lntup | grep daonode

# 查看 TCP BBR
sysctl net.ipv4.tcp_congestion_control

# 查看 UDP 错包和缓冲错误
netstat -su

# 查看 CPU、内存和进程线程
top -H -p "$(pidof daonode)"

# 查看最近日志
journalctl -u daonode -n 100 --no-pager
```

GeoIP/GeoSite 数据文件由发布包和安装脚本自动安装到 `/etc/daonode`。`geoip:private` 即使没有数据文件也有内置私网规则；其他 `geoip:<code>` 和 `geosite:<code>` 规则需要对应数据文件。

HTTP/HTTPS 是 Mieru 客户端提供的本地代理接口，服务端接收的是 Mieru 官方加密后的 SOCKS5 请求，不需要在 daonode 上额外监听 HTTP/HTTPS 端口。XChaCha20-Poly1305、随机填充、Traffic Pattern 和重放检测均由 Mieru 官方 Server API 执行。

排查顺序：

1. 确认客户端与服务端端口、协议、用户名和密码一致。
2. 确认 TCP/UDP 防火墙规则已经放行。
3. 确认客户端与服务端 MTU 一致。
4. TCP 检查 BBR，UDP 检查 `netstat -su` 是否持续增加 receive buffer error。
5. 将 Multiplexing 降到 `LOW`，临时清空 Traffic Pattern 后复测。
6. 对比 TCP 和 UDP，选择当前线路实际更快的一种。

## 构建与测试

需要 Go `1.26.1`，并启用 `jsonv2` experiment：

```bash
GOEXPERIMENT=jsonv2 go test ./...
GOEXPERIMENT=jsonv2 go build -trimpath \
  -ldflags "-X 'github.com/limo13660/daonode/cmd.version=v0.1.0' -s -w -buildid=" \
  -o daonode
```

## 上游文档

- [Mieru 协议说明](https://github.com/enfein/mieru/blob/v3.34.1/docs/protocol.zh_CN.md)
- [Mieru 服务端安装与 BBR](https://github.com/enfein/mieru/blob/v3.34.1/docs/server-install.zh_CN.md)
- [Mieru 运维与速度排查](https://github.com/enfein/mieru/blob/v3.34.1/docs/operation.zh_CN.md)
- [Mieru Traffic Pattern](https://github.com/enfein/mieru/blob/v3.34.1/docs/traffic-pattern.zh_CN.md)

## 许可证

原有 daonode 代码保留 MPL-2.0 条款。由于发布的 daonode 二进制直接链接 GPL-3.0 许可的 Mieru，组合二进制发布时需要同时遵守 GPL-3.0。详见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) 和 [LICENSE-GPL-3.0](LICENSE-GPL-3.0)。

## Stars

[![Stargazers over time](https://starchart.cc/limo13660/daonode.svg?variant=adaptive)](https://starchart.cc/limo13660/daonode)
