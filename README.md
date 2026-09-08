# 短信UART转发器

基于 合宙Air780 XXX 系列设备的短信转发系统，支持接收短信并通过串口转发到上位机。

[项目说明](https://blog.typesafe.cn/posts/air780e-giffgaff/)

**已测试设备**

- Air780EHV
- Air780EHM
- Air780E (可以使用，但属于过时设备，不建议购买)
- Air780EPV (可以使用，但属于过时设备，不建议购买)


## 🌟 功能特性

- 短信转发
- 短信记录
- 发送短信
- 来电通知
- 支持钉钉、企业微信、飞书、自定义 webhook、邮箱通知
- 计划任务发送短信
- 同时管理多台 Air780，短信、状态和计划任务按设备隔离
- 串口断线重连、接收 ACK/去重和发送结果超时保护

## 多设备配置

每台 Air780 都需要烧录仓库中的 `main.lua`，然后在 `config.yaml` 中为每个 USB 串口配置稳定且唯一的设备 ID：

```yaml
App:
  Serial:
    Devices:
      - ID: "air780-1"
        Name: "主卡"
        Port: "/dev/ttyUSB0"
        Enabled: true
      - ID: "air780-2"
        Name: "备用卡"
        Port: "/dev/ttyUSB1"
        Enabled: true
```

Docker 部署时还需要将所有串口映射到容器：

```yaml
devices:
  - /dev/ttyUSB0:/dev/ttyUSB0
  - /dev/ttyUSB1:/dev/ttyUSB1
```

Web 顶部可以切换当前设备。发送短信、查看状态、短信会话、飞行模式和计划任务都会使用当前选择；计划任务会固定保存目标 `deviceId`。

建议所有设备同步烧录新版 `main.lua`。新版主机连接后会主动协商开启接收 ACK；与旧版主机或旧版 Lua 组合时会自动退回原有无 ACK 模式。

未配置 `Devices` 时仍兼容旧版 `App.Serial.Port`，系统会创建 ID 为 `default` 的设备，并在升级时把旧短信和计划任务迁移到该设备。

主要 API：

- `GET /api/serial/devices`：返回所有设备及状态
- `GET /api/serial/status?deviceId=air780-1`：查询指定设备
- `POST /api/serial/sms`：请求体增加可选的 `deviceId`
- `POST /api/serial/flymode`、`POST /api/serial/reboot`：请求体支持 `deviceId`

## 截图

![s1.png](screenshots/s1.png)
![s2.png](screenshots/s2.png)
![s3.png](screenshots/s3.png)
![s4.png](screenshots/s4.png)
![s5.png](screenshots/s5.png)

## 🚀 快速开始

----

**重要说明：合宙某些版本的固件有 bug，如果遇到不能收发短信的情况，请换一个固件。**

**重要说明：合宙某些版本的固件有 bug，如果遇到不能收发短信的情况，请换一个固件。**

**重要说明：合宙某些版本的固件有 bug，如果遇到不能收发短信的情况，请换一个固件。**

----

### 1. 硬件准备

**设备准备**：
- 插入有效的SIM卡
- 通过USB连接电脑

### 2. 烧录 Lua 脚本

使用 [**LuaTools**](https://docs.openluat.com/air780epm/common/Luatools/) 烧录 `main.lua` 脚本，第一次烧录需要点击 「下载底层和脚本」

![write.png](screenshots/write.png)

### 3. 测试

![test.png](screenshots/test.png)

### 4. 把设备插入到你的小主机等 Linux USB上


### 5. 运行上位机程序

#### docker 方式安装

```shell
# 创建空目录
mkdir /opt/uart_sms_forwarder
# 下载 docker-compose.yml 文件
wget https://raw.githubusercontent.com/dushixiang/uart_sms_forwarder/main/docker-compose.yml -O /opt/uart_sms_forwarder/docker-compose.yml
# 下载 config.example.yaml 文件
wget https://raw.githubusercontent.com/dushixiang/uart_sms_forwarder/main/config.example.yaml -O /opt/uart_sms_forwarder/config.yaml
```

修改 `docker-compose.yml` 和 `config.yaml` 文件，主要是映射 USB 路径和修改密码。

启动服务

```shell
docker-compose up -d
```

打开浏览器访问 8080 端口。

----

#### 原生方式安装

下载

```shell
wget https://github.com/dushixiang/uart_sms_forwarder/releases/latest/download/uart_sms_forwarder-linux-amd64.tar.gz
```

解压
```bash
tar -zxvf uart_sms_forwarder-linux-amd64.tar.gz -C /opt/
mv /opt/uart_sms_forwarder-linux-amd64 /opt/uart_sms_forwarder
```

创建系统服务

```shell
cat <<EOF > /etc/systemd/system/uart_sms_forwarder.service
[Unit]
Description=uart_sms_forwarder service
After=network.target

[Service]
User=root
WorkingDirectory=/opt/uart_sms_forwarder
ExecStart=/opt/uart_sms_forwarder/uart_sms_forwarder
TimeoutSec=0
RestartSec=10
Restart=always
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
```

创建 sqllite 目录

```shell
mkdir /opt/uart_sms_forwarder/data
```

启动服务

```shell
systemctl daemon-reload
systemctl enable uart_sms_forwarder
systemctl start uart_sms_forwarder
```

打开浏览器访问 8080 端口。

修改密码等配置项，请参考 [config.example.yaml](config.example.yaml) 文件。


