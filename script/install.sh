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
        need_install_yum wget curl unzip tar cronie socat ca-certificates pv
        update-ca-trust force-enable >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"alpine" ]]; then
        need_install_apk wget curl unzip tar socat ca-certificates pv
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"debian" ]]; then
        need_install_apt wget curl unzip tar cron socat ca-certificates pv
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"ubuntu" ]]; then
        need_install_apt wget curl unzip tar cron socat ca-certificates pv
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"arch" ]]; then
        echo "更新包数据库..."
        pacman -Sy --noconfirm >/dev/null 2>&1
        # --needed 会跳过已安装的包，非常高效
        echo "安装必需的包..."
        pacman -S --noconfirm --needed wget curl unzip tar cronie socat ca-certificates pv >/dev/null 2>&1
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

install_daonode() {
    local version_param="$1"
    if [[ -e /usr/local/daonode/ ]]; then
        rm -rf /usr/local/daonode/
    fi

    mkdir /usr/local/daonode/ -p
    cd /usr/local/daonode/

    if  [[ -z "$version_param" ]] ; then
        last_version=$(curl -Ls "https://api.github.com/repos/limo13660/daonode/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
        if [[ ! -n "$last_version" ]]; then
            echo -e "${red}检测 daonode 版本失败，可能是超出 Github API 限制，请稍后再试，或手动指定 daonode 版本安装${plain}"
            exit 1
        fi
        echo -e "${green}检测到最新版本：${last_version}，开始安装...${plain}"
        url="https://github.com/limo13660/daonode/releases/download/${last_version}/daonode-linux-${arch}.zip"
        if ! curl -fL --retry 3 --connect-timeout 15 "$url" | pv -W -N "下载进度" > /usr/local/daonode/daonode-linux.zip; then
            echo -e "${red}下载 daonode 失败，请确保你的服务器能够下载 Github 的文件${plain}"
            exit 1
        fi
    else
    last_version=$version_param
        url="https://github.com/limo13660/daonode/releases/download/${last_version}/daonode-linux-${arch}.zip"
        if ! curl -fL --retry 3 --connect-timeout 15 "$url" | pv -W -N "下载进度" > /usr/local/daonode/daonode-linux.zip; then
            echo -e "${red}下载 daonode $1 失败，请确保此版本存在${plain}"
            exit 1
        fi
    fi

    if ! unzip -tq daonode-linux.zip >/dev/null 2>&1; then
        echo -e "${red}下载的安装包不是有效 ZIP 文件，请确认 Release 和架构名称是否正确${plain}"
        exit 1
    fi
    if ! unzip -o daonode-linux.zip; then
        echo -e "${red}解压 daonode 安装包失败${plain}"
        exit 1
    fi
    rm daonode-linux.zip -f
    chmod +x daonode
    mkdir /etc/daonode/ -p
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
ExecStart=/usr/local/daonode/daonode server
Restart=always
RestartSec=10

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


    curl -o /usr/bin/daonode -Ls https://raw.githubusercontent.com/limo13660/daonode/main/script/daonode.sh
    chmod +x /usr/bin/daonode

    cd $cur_dir
    rm -f install.sh
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
    curl -fsS --max-time 10 "https://api.v-50.me/counter" || true

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
install_daonode "$VERSION_ARG"
