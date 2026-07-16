#!/bin/bash

set -o pipefail

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

cur_dir=$(pwd)

# check root
[[ $EUID -ne 0 ]] && echo -e "${red}错误：${plain} 必须使用root用户运行此脚本！\n" && exit 1

# check os
if [[ -f /etc/redhat-release ]]; then
    release="centos"
elif cat /etc/issue | grep -Eqi "alpine"; then
    release="alpine"
elif cat /etc/issue | grep -Eqi "debian"; then
    release="debian"
elif cat /etc/issue | grep -Eqi "ubuntu"; then
    release="ubuntu"
elif cat /etc/issue | grep -Eqi "centos|red hat|redhat|rocky|alma|oracle linux"; then
    release="centos"
elif cat /proc/version | grep -Eqi "debian"; then
    release="debian"
elif cat /proc/version | grep -Eqi "ubuntu"; then
    release="ubuntu"
elif cat /proc/version | grep -Eqi "centos|red hat|redhat|rocky|alma|oracle linux"; then
    release="centos"
elif cat /proc/version | grep -Eqi "arch"; then
    release="arch"
else
    echo -e "${red}未检测到系统版本，请联系脚本作者！${plain}\n" && exit 1
fi

########################
# 参数解析
########################
VERSION_ARG=""
API_HOST_ARG=""
NODE_ID_ARG=""
API_KEY_ARG=""

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --api-host)
                API_HOST_ARG="$2"; shift 2 ;;
            --node-id)
                NODE_ID_ARG="$2"; shift 2 ;;
            --api-key)
                API_KEY_ARG="$2"; shift 2 ;;
            -h|--help)
                echo "用法: $0 [版本号] [--api-host URL] [--node-id ID] [--api-key KEY]"
                exit 0 ;;
            --*)
                echo "未知参数: $1"; exit 1 ;;
            *)
                # 兼容第一个位置参数作为版本号
                if [[ -z "$VERSION_ARG" ]]; then
                    VERSION_ARG="$1"; shift
                else
                    shift
                fi ;;
        esac
    done
}

arch=$(uname -m)

if [[ $arch == "x86_64" || $arch == "x64" || $arch == "amd64" ]]; then
    arch="64"
elif [[ $arch == "aarch64" || $arch == "arm64" ]]; then
    arch="arm64-v8a"
elif [[ $arch == "s390x" ]]; then
    arch="s390x"
else
    arch="64"
    echo -e "${red}检测架构失败，使用默认架构: ${arch}${plain}"
fi

if [ "$(getconf WORD_BIT)" != '32' ] && [ "$(getconf LONG_BIT)" != '64' ] ; then
    echo "本软件不支持 32 位系统(x86)，请使用 64 位系统(x86_64)，如果检测有误，请联系作者"
    exit 2
fi

# os version
if [[ -f /etc/os-release ]]; then
    os_version=$(awk -F'[= ."]' '/VERSION_ID/{print $3}' /etc/os-release)
fi
if [[ -z "$os_version" && -f /etc/lsb-release ]]; then
    os_version=$(awk -F'[= ."]+' '/DISTRIB_RELEASE/{print $2}' /etc/lsb-release)
fi

if [[ x"${release}" == x"centos" ]]; then
    if [[ ${os_version} -le 6 ]]; then
        echo -e "${red}请使用 CentOS 7 或更高版本的系统！${plain}\n" && exit 1
    fi
elif [[ x"${release}" == x"ubuntu" ]]; then
    if [[ ${os_version} -lt 16 ]]; then
        echo -e "${red}请使用 Ubuntu 16 或更高版本的系统！${plain}\n" && exit 1
    fi
elif [[ x"${release}" == x"debian" ]]; then
    if [[ ${os_version} -lt 8 ]]; then
        echo -e "${red}请使用 Debian 8 或更高版本的系统！${plain}\n" && exit 1
    fi
fi

install_base() {
    # 优化版本：批量检查和安装包，减少系统调用
    need_install_apt() {
        local packages=("$@")
        local missing=()
        
        # 批量检查已安装的包
        local installed_list=$(dpkg-query -W -f='${Package}\n' 2>/dev/null | sort)
        
        for p in "${packages[@]}"; do
            if ! echo "$installed_list" | grep -q "^${p}$"; then
                missing+=("$p")
            fi
        done
        
        if [[ ${#missing[@]} -gt 0 ]]; then
            echo "安装缺失的包: ${missing[*]}"
            apt-get update -y >/dev/null 2>&1
            DEBIAN_FRONTEND=noninteractive apt-get install -y "${missing[@]}" >/dev/null 2>&1
        fi
    }

    need_install_yum() {
        local packages=("$@")
        local missing=()
        
        # 批量检查已安装的包
        local installed_list=$(rpm -qa --qf '%{NAME}\n' 2>/dev/null | sort)
        
        for p in "${packages[@]}"; do
            if ! echo "$installed_list" | grep -q "^${p}$"; then
                missing+=("$p")
            fi
        done
        
        if [[ ${#missing[@]} -gt 0 ]]; then
            echo "安装缺失的包: ${missing[*]}"
            yum install -y "${missing[@]}" >/dev/null 2>&1
        fi
    }

    need_install_apk() {
        local packages=("$@")
        local missing=()
        
        # 批量检查已安装的包
        local installed_list=$(apk info 2>/dev/null | sort)
        
        for p in "${packages[@]}"; do
            if ! echo "$installed_list" | grep -q "^${p}$"; then
                missing+=("$p")
            fi
        done
        
        if [[ ${#missing[@]} -gt 0 ]]; then
            echo "安装缺失的包: ${missing[*]}"
            apk add --no-cache "${missing[@]}" >/dev/null 2>&1
        fi
    }

    # 一次性安装所有必需的包
    if [[ x"${release}" == x"centos" ]]; then
        # 检查并安装 epel-release
        if ! rpm -q epel-release >/dev/null 2>&1; then
            echo "安装 EPEL 源..."
            yum install -y epel-release >/dev/null 2>&1
        fi
        need_install_yum wget curl unzip tar cronie socat ca-certificates
        update-ca-trust force-enable >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"alpine" ]]; then
        need_install_apk wget curl unzip tar socat ca-certificates
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"debian" ]]; then
        need_install_apt wget curl unzip tar cron socat ca-certificates
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"ubuntu" ]]; then
        need_install_apt wget curl unzip tar cron socat ca-certificates
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"arch" ]]; then
        echo "更新包数据库..."
        pacman -Sy --noconfirm >/dev/null 2>&1
        # --needed 会跳过已安装的包，非常高效
        echo "安装必需的包..."
        pacman -S --noconfirm --needed wget curl unzip tar cronie socat ca-certificates >/dev/null 2>&1
    fi
}

optimize_network() {
    if ! command -v sysctl >/dev/null 2>&1; then
        return
    fi

    mkdir -p /etc/sysctl.d
    cat > /etc/sysctl.d/99-daonode-network.conf <<EOF
net.core.rmem_max = 33554432
net.core.wmem_max = 33554432
net.core.rmem_default = 1048576
net.core.wmem_default = 1048576
net.core.netdev_max_backlog = 32768
net.ipv4.udp_rmem_min = 16384
net.ipv4.udp_wmem_min = 16384
EOF

    # Mieru officially recommends BBR for TCP. Its UDP transport already
    # implements BBR inside the protocol and does not use this kernel setting.
    # Write BBR as the default and keep the module loaded across reboots.
    mkdir -p /etc/modules-load.d
    printf '%s\n' tcp_bbr > /etc/modules-load.d/daonode-bbr.conf
    if command -v modprobe >/dev/null 2>&1; then
        modprobe tcp_bbr >/dev/null 2>&1 || true
    fi
    cat >> /etc/sysctl.d/99-daonode-network.conf <<EOF
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
EOF

    rm -f /etc/sysctl.d/99-daonode-udp.conf
    if ! sysctl -p /etc/sysctl.d/99-daonode-network.conf >/dev/null 2>&1; then
        echo -e "${yellow}当前内核可能不支持 BBR，请升级内核后检查 net.ipv4.tcp_available_congestion_control${plain}"
    fi
    if [[ "$(sysctl -n net.ipv4.tcp_congestion_control 2>/dev/null || true)" != "bbr" ]]; then
        echo -e "${yellow}TCP BBR 未能立即启用；daonode 已保留默认配置，重启或升级内核后会再次尝试${plain}"
    fi
}

# 0: running, 1: not running, 2: not installed
check_status() {
    if [[ ! -f /usr/local/daonode/daonode ]]; then
        return 2
    fi
    if [[ x"${release}" == x"alpine" ]]; then
        temp=$(service daonode status | awk '{print $3}')
        if [[ x"${temp}" == x"started" ]]; then
            return 0
        else
            return 1
        fi
    else
        temp=$(systemctl status daonode | grep Active | awk '{print $3}' | cut -d "(" -f2 | cut -d ")" -f1)
        if [[ x"${temp}" == x"running" ]]; then
            return 0
        else
            return 1
        fi
    fi
}

generate_daonode_config() {
        local api_host="$1"
        local node_id="$2"
        local api_key="$3"

        mkdir -p /etc/daonode >/dev/null 2>&1
        cat > /etc/daonode/config.json <<EOF
{
    "Log": {
        "Level": "warning",
        "Output": "",
        "Access": "none"
    },
    "Nodes": [
        {
            "ApiHost": "${api_host}",
            "NodeID": ${node_id},
            "ApiKey": "${api_key}",
            "Timeout": 15
        }
    ]
}
EOF
        echo -e "${green}DaoNode 配置文件生成完成,正在重新启动服务${plain}"
        if [[ x"${release}" == x"alpine" ]]; then
            service daonode restart
        else
            systemctl restart daonode
        fi
        sleep 2
        check_status
        echo -e ""
        if [[ $? == 0 ]]; then
            echo -e "${green}daonode 重启成功${plain}"
        else
            echo -e "${red}daonode 可能启动失败，请使用 daonode log 查看日志信息${plain}"
        fi
}

download_release() {
    local url="$1"
    local destination="$2"
    local partial="${destination}.part"
    local source
    local downloaded=0
    local curl_args=(
        -fL
        --retry 0
        --connect-timeout 15
        --max-time 300
        --speed-limit 1024
        --speed-time 60
        --progress-bar
    )
    local sources=("$url")

    rm -f "$partial"
    echo -e "${yellow}安装包约 24 MiB，低速网络可能需要数分钟；下载最长等待 30 分钟。${plain}"
    echo "Download timeout is 5 minutes; partial downloads resume automatically."
    for source in "${sources[@]}"; do
        echo "Download source: $source"
        if [[ -s "$partial" ]]; then
            if curl "${curl_args[@]}" --continue-at - -o "$partial" "$source"; then
                if unzip -tq "$partial" >/dev/null 2>&1; then
                    downloaded=1
                    break
                fi
                echo "Resumed data is not a valid release archive; retrying this source from the beginning."
            else
                echo "Resume failed; retrying this source from the beginning."
            fi
            rm -f "$partial"
        fi
        if curl "${curl_args[@]}" -o "$partial" "$source"; then
            if unzip -tq "$partial" >/dev/null 2>&1; then
                downloaded=1
                break
            fi
            echo "Downloaded data is not a valid release archive."
            rm -f "$partial"
        else
            echo "Download source failed."
        fi
    done
    if [[ $downloaded -ne 1 ]]; then
        rm -f "$partial"
        return 1
    fi
    if [[ ! -s "$partial" ]]; then
        rm -f "$partial"
        return 1
    fi
    mv -f "$partial" "$destination"
}

install_daonode() {
    local version_param="$1"
    local install_dir="/usr/local/daonode"
    local previous_dir="/usr/local/daonode.previous"
    local archive
    local stage_dir
    local manager_tmp=""

    archive=$(mktemp /tmp/daonode-linux.XXXXXX) || exit 1
    stage_dir=$(mktemp -d /usr/local/daonode.new.XXXXXX) || {
        rm -f "$archive"
        exit 1
    }

    if  [[ -z "$version_param" ]] ; then
        last_version=$(curl -fsSL --retry 2 --retry-max-time 120 --connect-timeout 15 --max-time 60 \
            "https://api.github.com/repos/limo13660/daonode/releases/latest" | \
            grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
        if [[ ! -n "$last_version" ]]; then
            echo -e "${red}检测 daonode 版本失败，可能是超出 Github API 限制，请稍后再试，或手动指定 daonode 版本安装${plain}"
            rm -rf "$stage_dir"
            rm -f "$archive"
            exit 1
        fi
        echo -e "${green}检测到最新版本：${last_version}，开始安装...${plain}"
        url="https://github.com/limo13660/daonode/releases/download/${last_version}/daonode-linux-${arch}.zip"
        if ! download_release "$url" "$archive"; then
            echo -e "${red}下载 daonode 失败，请确保你的服务器能够下载 Github 的文件${plain}"
            rm -rf "$stage_dir"
            rm -f "$archive"
            exit 1
        fi
    else
    last_version=$version_param
        url="https://github.com/limo13660/daonode/releases/download/${last_version}/daonode-linux-${arch}.zip"
        if ! download_release "$url" "$archive"; then
            echo -e "${red}下载 daonode $1 失败，请确保此版本存在${plain}"
            rm -rf "$stage_dir"
            rm -f "$archive"
            exit 1
        fi
    fi

    if ! unzip -tq "$archive" >/dev/null 2>&1; then
        echo -e "${red}下载的安装包不是有效 ZIP 文件，请确认 Release 和架构名称是否正确${plain}"
        rm -rf "$stage_dir"
        rm -f "$archive"
        exit 1
    fi
    if ! unzip -oq "$archive" -d "$stage_dir"; then
        echo -e "${red}解压 daonode 安装包失败${plain}"
        rm -rf "$stage_dir"
        rm -f "$archive"
        exit 1
    fi
    rm -f "$archive"
    if [[ ! -f "$stage_dir/daonode" ]]; then
        echo -e "${red}安装包中缺少 daonode 可执行文件${plain}"
        rm -rf "$stage_dir"
        exit 1
    fi
    chmod 0755 "$stage_dir"
    chmod +x "$stage_dir/daonode"

    rm -rf "$previous_dir"
    if [[ -d "$install_dir" ]]; then
        mv "$install_dir" "$previous_dir"
    fi
    if ! mv "$stage_dir" "$install_dir"; then
        echo -e "${red}替换 daonode 安装目录失败${plain}"
        if [[ -d "$previous_dir" ]]; then
            mv "$previous_dir" "$install_dir"
        fi
        exit 1
    fi

    cd "$install_dir" || exit 1
    chmod +x daonode
    mkdir /etc/daonode/ -p
    cat <<'EOF' > /usr/local/daonode/count-start.sh
#!/bin/sh

count_file="/etc/daonode/run_count"
count=0
if [ -f "$count_file" ]; then
    count=$(cat "$count_file" 2>/dev/null)
fi
case "$count" in
    ''|*[!0-9]*) count=0 ;;
esac
count=$((count + 1))
printf '%s\n' "$count" > "$count_file"
EOF
    chmod +x /usr/local/daonode/count-start.sh
    # Route groups use the same GeoIP/GeoSite dat format as v2node.
    for data in geoip.dat geosite.dat; do
        if [[ -f "$data" ]]; then
            install -m 0644 "$data" "/etc/daonode/$data"
        fi
    done
    if [[ x"${release}" == x"alpine" ]]; then
        rm /etc/init.d/daonode -f
        cat <<EOF > /etc/init.d/daonode
#!/sbin/openrc-run

name="daonode"
description="daonode"

command="/usr/local/daonode/daonode"
command_args="server"
command_user="root"

pidfile="/run/daonode.pid"
command_background="yes"

start_pre() {
        /usr/local/daonode/count-start.sh
}

depend() {
        need net
}
EOF
        chmod +x /etc/init.d/daonode
        rc-update add daonode default
        echo -e "${green}daonode ${last_version}${plain} 安装完成，已设置开机自启"
    else
        rm /etc/systemd/system/daonode.service -f
        cat <<EOF > /etc/systemd/system/daonode.service
[Unit]
Description=daonode Service
After=network.target nss-lookup.target
Wants=network.target

[Service]
User=root
Group=root
Type=simple
LimitAS=infinity
LimitRSS=infinity
LimitCORE=infinity
LimitNOFILE=999999
WorkingDirectory=/usr/local/daonode/
ExecStartPre=/usr/local/daonode/count-start.sh
ExecStart=/usr/local/daonode/daonode server
Restart=always
RestartSec=2
TimeoutStopSec=15
KillMode=control-group

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
        systemctl stop daonode
        systemctl enable daonode
        echo -e "${green}daonode ${last_version}${plain} 安装完成，已设置开机自启"
    fi

    if [[ ! -f /etc/daonode/config.json ]]; then
        # 如果通过 CLI 传入了完整参数，则直接生成配置并跳过交互
        if [[ -n "$API_HOST_ARG" && -n "$NODE_ID_ARG" && -n "$API_KEY_ARG" ]]; then
            generate_daonode_config "$API_HOST_ARG" "$NODE_ID_ARG" "$API_KEY_ARG"
            echo -e "${green}已根据参数生成 /etc/daonode/config.json${plain}"
            first_install=false
        else
            first_install=true
        fi
    else
        if [[ x"${release}" == x"alpine" ]]; then
            service daonode start
        else
            systemctl start daonode
        fi
        sleep 2
        check_status
        echo -e ""
        if [[ $? == 0 ]]; then
            echo -e "${green}daonode 重启成功${plain}"
        else
            echo -e "${red}daonode 可能启动失败，请使用 daonode log 查看日志信息${plain}"
        fi
        first_install=false
    fi


    manager_tmp=$(mktemp /tmp/daonode-manager.XXXXXX) || true
    if [[ -n "$manager_tmp" ]] && curl -fL --retry 2 --retry-max-time 120 --connect-timeout 15 --max-time 120 -sS \
        -o "$manager_tmp" https://raw.githubusercontent.com/limo13660/daonode/main/script/daonode.sh; then
        install -m 0755 "$manager_tmp" /usr/bin/daonode
    else
        echo -e "${yellow}管理脚本更新失败，保留现有 /usr/bin/daonode${plain}"
    fi
    if [[ -n "$manager_tmp" ]]; then
        rm -f "$manager_tmp"
    fi

    rm -rf "$previous_dir"

    cd "$cur_dir" || exit 1
    echo "------------------------------------------"
    echo -e "管理脚本使用方法: "
    echo "------------------------------------------"
    echo "daonode              - 显示管理菜单 (功能更多)"
    echo "daonode start        - 启动 daonode"
    echo "daonode stop         - 停止 daonode"
    echo "daonode restart      - 重启 daonode"
    echo "daonode status       - 查看 daonode 状态"
    echo "daonode enable       - 设置 daonode 开机自启"
    echo "daonode disable      - 取消 daonode 开机自启"
    echo "daonode log          - 查看 daonode 日志"
    echo "daonode generate     - 生成 daonode 配置文件"
    echo "daonode update       - 更新 daonode"
    echo "daonode update x.x.x - 更新 daonode 指定版本"
    echo "daonode install      - 安装 daonode"
    echo "daonode uninstall    - 卸载 daonode"
    echo "daonode version      - 查看 daonode 版本"
    echo "------------------------------------------"
    if [[ $first_install == true ]]; then
        read -rp "检测到你为第一次安装 daonode，是否自动生成 /etc/daonode/config.json？(y/n): " if_generate
        if [[ "$if_generate" =~ ^[Yy]$ ]]; then
            # 交互式收集参数，提供示例默认值
            read -rp "面板API地址[格式: https://example.com/]: " api_host
            api_host=${api_host:-https://example.com/}
            read -rp "节点ID: " node_id
            node_id=${node_id:-1}
            read -rp "节点通讯密钥: " api_key

            # 生成配置文件（覆盖可能从包中复制的模板）
            generate_daonode_config "$api_host" "$node_id" "$api_key"
        else
            echo "${green}已跳过自动生成配置。如需后续生成，可执行: daonode generate${plain}"
        fi
    fi
}

parse_args "$@"
echo -e "${green}开始安装${plain}"
install_base
optimize_network
install_daonode "$VERSION_ARG"
