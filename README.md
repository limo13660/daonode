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
- Mieru User Hint 强制校验
- 用户动态增删、限速、在线状态和流量上报
- DaoBoard 路由组、DNS 规则、域名/IP/端口/协议阻断
- 兼容 v2node 的 `geoip.dat`、`geosite.dat` 路由数据格式
- 配置热更新和安全退出
- Linux TCP BBR 与 UDP socket 缓冲优化
- Linux amd64、arm64、s390x 构建

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
| 强制用户提示 | 使用客户端发送的 User Hint 加速多用户解密；开启后旧客户端会被拒绝 |

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

附加绑定是“主端口之外的绑定列表”，不是替代主端口。每个对象的 `port` 与 `protocol` 在客户端配置中按相同位置配对；使用端口范围时，客户端和服务端范围长度必须相同。没有额外端口时直接保留默认值 `[]` 即可。

## User Hint

从 Mieru v3.31.0 开始，官方客户端会在加密握手中携带 User Hint。服务端可以先用这个提示定位用户名，再进行完整解密，用户较多时能显著减少 CPU 消耗。`强制用户提示` 对应官方 `advancedSettings.userHintIsMandatory`：

- 关闭：兼容旧客户端，但没有 User Hint 的请求会增加服务端解密尝试。
- 开启：只接受支持 User Hint 的 Mieru 客户端；旧客户端会连接失败。

建议确认所有客户端已升级到支持 User Hint 的版本后再开启。

## 官方性能建议

### 1. 优先测试 TCP

Mieru 官方说明 TCP 通常比 UDP 更快，因为 UDP 数据包需要更多解密尝试。普通网络建议先使用 TCP；只有 TCP 在高峰期被 QoS、丢包或限速时，再测试 UDP。

### 2. TCP 使用 BBR

官方建议 Linux TCP 启用 BBR。安装脚本会在内核支持时加载 `tcp_bbr`，并设置：

```text
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
```

检查结果：

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
