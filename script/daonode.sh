#!/bin/bash

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

confirm() {
    if [[ $# > 1 ]]; then
        echo && read -rp "$1 [默认$2]: " temp
        if [[ x"${temp}" == x"" ]]; then
            temp=$2
        fi
    else
        read -rp "$1 [y/n]: " temp
    fi
    if [[ x"${temp}" == x"y" || x"${temp}" == x"Y" ]]; then
        return 0
    else
        return 1
    fi
}

confirm_restart() {
    confirm "是否重启daonode" "y"
    if [[ $? == 0 ]]; then
        restart
    else
        show_menu
    fi
}

before_show_menu() {
    echo && echo -n -e "${yellow}按回车返回主菜单: ${plain}" && read temp
    show_menu
}

run_installer() {
    local installer_tmp
    local status

    installer_tmp=$(mktemp /tmp/daonode-installer.XXXXXX) || return 1
    if ! curl -fL --retry 2 --retry-max-time 120 --connect-timeout 15 --max-time 120 -sS \
        -o "$installer_tmp" https://raw.githubusercontent.com/limo13660/daonode/main/script/install.sh; then
        rm -f "$installer_tmp"
        echo -e "${red}下载安装脚本失败，请检查本机能否连接 Github${plain}"
        return 1
    fi
    bash "$installer_tmp" "$@"
    status=$?
    rm -f "$installer_tmp"
    return $status
}

install() {
    if run_installer; then
        if [[ $# == 0 ]]; then
            start
        else
            start 0
        fi
        return 0
    fi
    return 1
}

update() {
    if [[ $# == 0 ]]; then
        echo && echo -n -e "输入指定版本(默认最新版): " && read version
    else
        version=$2
    fi
    if run_installer "$version"; then
        echo -e "${green}更新完成，已自动重启 daonode，请使用 daonode log 查看运行日志${plain}"
        exit
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
    return 1
}

config() {
    echo "daonode在修改配置后会自动尝试重启"
    vi /etc/daonode/config.json
    sleep 2
    restart
    check_status
    case $? in
        0)
            echo -e "daonode状态: ${green}已运行${plain}"
            ;;
        1)
            echo -e "检测到您未启动daonode或daonode自动重启失败，是否查看日志？[Y/n]" && echo
            read -e -rp "(默认: y):" yn
            [[ -z ${yn} ]] && yn="y"
            if [[ ${yn} == [Yy] ]]; then
               show_log
            fi
            ;;
        2)
            echo -e "daonode状态: ${red}未安装${plain}"
    esac
}

uninstall() {
    confirm "确定要卸载 daonode 吗?" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi
    if [[ x"${release}" == x"alpine" ]]; then
        service daonode stop
        rc-update del daonode
        rm /etc/init.d/daonode -f
    else
        systemctl stop daonode
        systemctl disable daonode
        rm /etc/systemd/system/daonode.service -f
        systemctl daemon-reload
        systemctl reset-failed
    fi
    rm /etc/daonode/ -rf
    rm /usr/local/daonode/ -rf

    echo ""
    echo -e "卸载成功，如果你想删除此脚本，则退出脚本后运行 ${green}rm /usr/bin/daonode -f${plain} 进行删除"
    echo ""

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

start() {
    check_status
    if [[ $? == 0 ]]; then
        echo ""
        echo -e "${green}daonode已运行，无需再次启动，如需重启请选择重启${plain}"
    else
        if [[ x"${release}" == x"alpine" ]]; then
            service daonode start
        else
            systemctl start daonode
        fi
        sleep 2
        check_status
        if [[ $? == 0 ]]; then
            echo -e "${green}daonode 启动成功，请使用 daonode log 查看运行日志${plain}"
        else
            echo -e "${red}daonode可能启动失败，请稍后使用 daonode log 查看日志信息${plain}"
        fi
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

stop() {
    if [[ x"${release}" == x"alpine" ]]; then
        service daonode stop
    else
        systemctl stop daonode
    fi
    sleep 2
    check_status
    if [[ $? == 1 ]]; then
        echo -e "${green}daonode 停止成功${plain}"
    else
        echo -e "${red}daonode停止失败，可能是因为停止时间超过了两秒，请稍后查看日志信息${plain}"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

restart() {
    if [[ x"${release}" == x"alpine" ]]; then
        service daonode restart
    else
        systemctl restart daonode
    fi
    sleep 2
    check_status
    if [[ $? == 0 ]]; then
        echo -e "${green}daonode 重启成功，请使用 daonode log 查看运行日志${plain}"
    else
        echo -e "${red}daonode可能启动失败，请稍后使用 daonode log 查看日志信息${plain}"
    fi
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

status() {
    if [[ x"${release}" == x"alpine" ]]; then
        service daonode status
    else
        systemctl status daonode --no-pager -l
    fi
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

enable() {
    if [[ x"${release}" == x"alpine" ]]; then
        rc-update add daonode
    else
        systemctl enable daonode
    fi
    if [[ $? == 0 ]]; then
        echo -e "${green}daonode 设置开机自启成功${plain}"
    else
        echo -e "${red}daonode 设置开机自启失败${plain}"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

disable() {
    if [[ x"${release}" == x"alpine" ]]; then
        rc-update del daonode
    else
        systemctl disable daonode
    fi
    if [[ $? == 0 ]]; then
        echo -e "${green}daonode 取消开机自启成功${plain}"
    else
        echo -e "${red}daonode 取消开机自启失败${plain}"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

show_log() {
    if [[ x"${release}" == x"alpine" ]]; then
        echo -e "${red}alpine系统暂不支持日志查看${plain}\n" && exit 1
    else
        journalctl -u daonode.service -e --no-pager -f
    fi
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

update_shell() {
    wget -O /usr/bin/daonode -N --no-check-certificate https://raw.githubusercontent.com/limo13660/daonode/main/script/daonode.sh
    if [[ $? != 0 ]]; then
        echo ""
        echo -e "${red}下载脚本失败，请检查本机能否连接 Github${plain}"
        before_show_menu
    else
        chmod +x /usr/bin/daonode
        echo -e "${green}升级脚本成功，请重新运行脚本${plain}" && exit 0
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

check_enabled() {
    if [[ x"${release}" == x"alpine" ]]; then
        temp=$(rc-update show | grep daonode)
        if [[ x"${temp}" == x"" ]]; then
            return 1
        else
            return 0
        fi
    else
        temp=$(systemctl is-enabled daonode)
        if [[ x"${temp}" == x"enabled" ]]; then
            return 0
        else
            return 1;
        fi
    fi
}

check_uninstall() {
    check_status
    if [[ $? != 2 ]]; then
        echo ""
        echo -e "${red}daonode已安装，请不要重复安装${plain}"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    else
        return 0
    fi
}

check_install() {
    check_status
    if [[ $? == 2 ]]; then
        echo ""
        echo -e "${red}请先安装daonode${plain}"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    else
        return 0
    fi
}

show_status() {
    check_status
    case $? in
        0)
            echo -e "daonode状态: ${green}已运行${plain}"
            show_enable_status
            show_backend_info
            ;;
        1)
            echo -e "daonode状态: ${yellow}未运行${plain}"
            show_enable_status
            show_backend_info
            ;;
        2)
            echo -e "daonode状态: ${red}未安装${plain}"
    esac
}

get_backend_version() {
    local binary="/usr/local/daonode/daonode"
    local backend_version
    if [[ ! -x "$binary" ]]; then
        echo "未安装"
        return
    fi
    backend_version=$("$binary" version 2>/dev/null | awk 'NR == 1 {print $2}')
    if [[ -z "$backend_version" ]]; then
        backend_version="未知"
    fi
    echo "$backend_version"
}

get_backend_run_count() {
    local count_file="/etc/daonode/run_count"
    local count
    if [[ -f "$count_file" ]]; then
        count=$(cat "$count_file" 2>/dev/null)
        if [[ "$count" =~ ^[0-9]+$ ]]; then
            echo "$count"
            return
        fi
    fi

    # Compatibility fallback for installations created before the persistent
    # backend counter was introduced.
    if [[ x"${release}" != x"alpine" ]] && command -v systemctl >/dev/null 2>&1; then
        count=$(systemctl show daonode -p NRestarts --value 2>/dev/null)
        [[ "$count" =~ ^[0-9]+$ ]] || count=0
        if systemctl is-active --quiet daonode 2>/dev/null; then
            count=$((count + 1))
        fi
        echo "$count"
        return
    fi

    check_status
    if [[ $? == 0 ]]; then
        echo "1"
    else
        echo "0"
    fi
}

show_backend_info() {
    local backend_version
    local backend_run_count
    backend_version=$(get_backend_version)
    backend_run_count=$(get_backend_run_count)
    echo -e "当前后端版本: ${green}${backend_version}${plain}"
    echo -e "当前后端累计运行次数: ${green}${backend_run_count}${plain}"
}

show_enable_status() {
    check_enabled
    if [[ $? == 0 ]]; then
        echo -e "是否开机自启: ${green}是${plain}"
    else
        echo -e "是否开机自启: ${red}否${plain}"
    fi
}

show_daonode_version() {
    echo -e "当前后端版本: ${green}$(get_backend_version)${plain}"
    echo ""
    if [[ $# == 0 ]]; then
        before_show_menu
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


generate_config_file() {
    # 交互式收集参数，提供示例默认值
    read -rp "面板API地址[格式: https://example.com/]: " api_host
    api_host=${api_host:-https://example.com/}
    read -rp "节点ID: " node_id
    node_id=${node_id:-1}
    read -rp "节点通讯密钥: " api_key

    # 生成配置文件（覆盖可能从包中复制的模板）
    generate_daonode_config "$api_host" "$node_id" "$api_key"
}

# 放开防火墙端口
open_ports() {
    systemctl stop firewalld.service 2>/dev/null
    systemctl disable firewalld.service 2>/dev/null
    setenforce 0 2>/dev/null
    ufw disable 2>/dev/null
    iptables -P INPUT ACCEPT 2>/dev/null
    iptables -P FORWARD ACCEPT 2>/dev/null
    iptables -P OUTPUT ACCEPT 2>/dev/null
    iptables -t nat -F 2>/dev/null
    iptables -t mangle -F 2>/dev/null
    iptables -F 2>/dev/null
    iptables -X 2>/dev/null
    netfilter-persistent save 2>/dev/null
    echo -e "${green}放开防火墙端口成功！${plain}"
}

show_usage() {
    echo "daonode 管理脚本使用方法: "
    echo "------------------------------------------"
    echo "daonode              - 显示管理菜单 (功能更多)"
    echo "daonode start        - 启动 daonode"
    echo "daonode stop         - 停止 daonode"
    echo "daonode restart      - 重启 daonode"
    echo "daonode status       - 查看 daonode 状态"
    echo "daonode enable       - 设置 daonode 开机自启"
    echo "daonode disable      - 取消 daonode 开机自启"
    echo "daonode log          - 查看 daonode 日志"
    echo "daonode x25519       - 生成 x25519 密钥"
    echo "daonode generate     - 生成 daonode 配置文件"
    echo "daonode update       - 更新 daonode"
    echo "daonode update x.x.x - 安装 daonode 指定版本"
    echo "daonode install      - 安装 daonode"
    echo "daonode uninstall    - 卸载 daonode"
    echo "daonode version      - 查看 daonode 版本"
    echo "------------------------------------------"
}

show_menu() {
    echo -e "
  ${green}daonode 后端管理脚本，${plain}${red}不适用于docker${plain}
--- https://github.com/limo13660/daonode ---
  ${green}0.${plain} 修改配置
————————————————
  ${green}1.${plain} 安装 daonode
  ${green}2.${plain} 更新 daonode
  ${green}3.${plain} 卸载 daonode
————————————————
  ${green}4.${plain} 启动 daonode
  ${green}5.${plain} 停止 daonode
  ${green}6.${plain} 重启 daonode
  ${green}7.${plain} 查看 daonode 状态
  ${green}8.${plain} 查看 daonode 日志
————————————————
  ${green}9.${plain} 设置 daonode 开机自启
  ${green}10.${plain} 取消 daonode 开机自启
————————————————
  ${green}11.${plain} 查看 daonode 版本
  ${green}12.${plain} 升级 daonode 维护脚本
  ${green}13.${plain} 生成 daonode 配置文件
  ${green}14.${plain} 放行 VPS 的所有网络端口
  ${green}15.${plain} 退出脚本
 "
 #后续更新可加入上方字符串中
    show_status
    echo && read -rp "请输入选择 [0-15]: " num

    case "${num}" in
        0) config ;;
        1) check_uninstall && install ;;
        2) check_install && update ;;
        3) check_install && uninstall ;;
        4) check_install && start ;;
        5) check_install && stop ;;
        6) check_install && restart ;;
        7) check_install && status ;;
        8) check_install && show_log ;;
        9) check_install && enable ;;
        10) check_install && disable ;;
        11) check_install && show_daonode_version ;;
        12) update_shell ;;
        13) generate_config_file ;;
        14) open_ports ;;
        15) exit ;;
        *) echo -e "${red}请输入正确的数字 [0-15]${plain}" ;;
    esac
}


if [[ $# > 0 ]]; then
    case $1 in
        "start") check_install 0 && start 0 ;;
        "stop") check_install 0 && stop 0 ;;
        "restart") check_install 0 && restart 0 ;;
        "status") check_install 0 && status 0 ;;
        "enable") check_install 0 && enable 0 ;;
        "disable") check_install 0 && disable 0 ;;
        "log") check_install 0 && show_log 0 ;;
        "update") check_install 0 && update 0 $2 ;;
        "config") config $* ;;
        "generate") generate_config_file ;;
        "install") check_uninstall 0 && install 0 ;;
        "uninstall") check_install 0 && uninstall 0 ;;
        "version") check_install 0 && show_daonode_version 0 ;;
        "update_shell") update_shell ;;
        *) show_usage
    esac
else
    show_menu
fi
