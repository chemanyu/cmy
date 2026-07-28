#!/bin/bash

# 配置信息
REMOTE_USER="root"
REMOTE_HOST="adx-s12.ms"
REMOTE_DIR="/usr/local/adt-go/ocpx_mcp"
REMOTE_LOG_DIR="/data/log/go/adt-go/ocpx_mcp"
LOCAL_BINARY="ocpx_mcp"
REMOTE_BINARY="ocpx_mcp"
SERVICE_FILE="ocpx_mcp.service"
TEMP_BINARY="ocpx_mcp.new"
CONFIG_FILE="etc/config.yaml"

# 交叉编译环境变量
export GOOS=linux
export GOARCH=amd64
export CGO_ENABLED=0

# 颜色输出
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
RED='\033[0;31m'
NC='\033[0m'

info() {
    echo -e "${GREEN}[INFO] $1${NC}"
}

warn() {
    echo -e "${YELLOW}[WARN] $1${NC}"
}

error() {
    echo -e "${RED}[ERROR] $1${NC}"
    exit 1
}

# 编译项目
info "开始交叉编译 Linux 二进制文件..."
go build -o $LOCAL_BINARY || error "编译失败"

# 在服务器上创建目录并设置权限
# 日志目录必须先建：service 里用的是 append:，目录不存在服务会起不来
info "准备服务器目录..."
ssh $REMOTE_USER@$REMOTE_HOST "mkdir -p $REMOTE_DIR/etc && chmod 755 $REMOTE_DIR && mkdir -p $REMOTE_LOG_DIR" || error "服务器目录准备失败"

# 同步文件到服务器
info "同步文件到服务器..."
scp $LOCAL_BINARY $REMOTE_USER@$REMOTE_HOST:$REMOTE_DIR/$TEMP_BINARY || error "文件同步失败"
scp $SERVICE_FILE $REMOTE_USER@$REMOTE_HOST:/etc/systemd/system/$SERVICE_FILE || error "服务配置文件同步失败"

# config.yaml 里有 Doris 密码和 MCP token：首次部署上传模板，之后一律保留线上的不动
NEED_CONFIG_EDIT=false
if ssh $REMOTE_USER@$REMOTE_HOST "test -f $REMOTE_DIR/$CONFIG_FILE"; then
    info "$CONFIG_FILE 已存在，保留不动（不覆盖线上凭据）"
else
    scp $CONFIG_FILE $REMOTE_USER@$REMOTE_HOST:$REMOTE_DIR/$CONFIG_FILE || error "配置文件同步失败"
    ssh $REMOTE_USER@$REMOTE_HOST "chmod 600 $REMOTE_DIR/$CONFIG_FILE" || error "配置文件权限设置失败"
    NEED_CONFIG_EDIT=true
fi

# 在服务器上执行替换和重启
info "替换文件并重启服务..."
ssh $REMOTE_USER@$REMOTE_HOST "cd $REMOTE_DIR && \
    chmod +x $TEMP_BINARY && \
    systemctl daemon-reload && \
    systemctl stop $SERVICE_FILE; \
    mv -f $TEMP_BINARY $REMOTE_BINARY && \
    systemctl enable $SERVICE_FILE && \
    systemctl start $SERVICE_FILE" || error "服务重启失败"

# 清理本地编译文件
rm -f $LOCAL_BINARY

if [ "$NEED_CONFIG_EDIT" = true ]; then
    warn "首次部署：$REMOTE_DIR/$CONFIG_FILE 是模板，Doris.Password 与 MCP.AuthToken 为空，服务连不上 Doris。"
    echo "  填完再重启："
    echo "    ssh $REMOTE_USER@$REMOTE_HOST vi $REMOTE_DIR/$CONFIG_FILE"
    echo "    ssh $REMOTE_USER@$REMOTE_HOST systemctl restart $SERVICE_FILE"
    exit 0
fi

info "部署完成！"
