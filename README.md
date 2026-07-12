# daonode
A V2Board-compatible node server with an extensible protocol runtime.
兼容 V2Board 接口的可扩展节点服务端，目前仅内置 Mieru 协议。

**注意：本项目需要搭配 [DaoBoard](https://github.com/limo13660/DaoBoard)。**

## 当前协议

- [Mieru](https://github.com/enfein/mieru) TCP / UDP

## 节点能力

daonode 保留与 v2node 相同的节点生命周期、面板通信、用户同步、限速、
流量上报以及证书管理框架。证书支持 file、self、HTTP-01 和 DNS-01 模式，
默认存放在 `/etc/daonode`。当前 Mieru 协议不使用 TLS 证书，因此不会为
Mieru 监听启动证书申请或续期；后续需要 TLS 的协议运行时可直接复用该能力。

## 许可证

项目原有代码保留 MPL-2.0 条款。由于 daonode 二进制直接链接 GPL-3.0
许可的 Mieru，组合二进制发布时须同时遵守 GPL-3.0。详见
`THIRD_PARTY_NOTICES.md` 与 `LICENSE-GPL-3.0`。

## 软件安装

### 一键安装

```
wget -N https://raw.githubusercontent.com/limo13660/daonode/main/script/install.sh && bash install.sh
```

## 构建
``` bash
GOEXPERIMENT=jsonv2 go build -v -o build_assets/daonode -trimpath -ldflags "-X 'github.com/limo13660/daonode/cmd.version=$version' -s -w -buildid="
```

## Stars 增长记录

[![Stargazers over time](https://starchart.cc/limo13660/daonode.svg?variant=adaptive)](https://starchart.cc/limo13660/daonode)
