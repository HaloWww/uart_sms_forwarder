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
- 同时管理多台 Air780，短信、状态和计划任务按 SIM 卡（ICCID）隔离
- 串口断线重连、接收 ACK/去重和发送结果超时保护

## 多设备配置

每台 Air780 都需要烧录仓库中的新版 `main.lua`，然后在 `config.yaml` 中为每个 USB 串口配置唯一的物理连接 ID：

```yaml
App:
  Serial:
    Devices:
      - ID: "air780-1"
        Name: "Air780 一号"
        Port: "/dev/ttyUSB0"
        Enabled: true
      - ID: "air780-2"
        Name: "Air780 二号"
        Port: "/dev/ttyUSB1"
        Enabled: true
```

Docker 部署时还需要将所有串口映射到容器：

```yaml
devices:
  - /dev/ttyUSB0:/dev/ttyUSB0
  - /dev/ttyUSB1:/dev/ttyUSB1
```

`Devices[].ID` 只表示模块当前连接在哪个串口，不再决定短信归属。系统从模块读取 ICCID，并生成稳定的 `simId`；Web 顶部按 SIM 切换，短信历史和计划任务都会固定保存目标 `simId`。把两张卡在两台 Air780 之间互换后，业务数据和计划任务仍跟随原 SIM，不会跟着串口或模块走。

身份字段的含义：

- **ICCID**：SIM 卡本身的固定编号，是短信和计划任务的业务主键。
- **IMSI**：运营商订阅身份，只作为辅助信息，不作为固定主键。
- **IMEI**：Air780 模块的硬件编号，只记录“这次由哪台模块处理”，不能代表 SIM 卡。

发送时主机会把目标 ICCID 下发给 Lua。Lua 会在入队和真正调用短信发送接口前再次读取并核对 ICCID；换卡、身份未确认或同一 ICCID 冲突时都会拒绝发送。已发现的 SIM 档案会保存在数据库里，所以 SIM 离线后仍可查看历史和编辑计划任务，但不能发送。

如果设备已把短信提交给基带、但最终回执时 SIM 身份发生变化或回执超时，记录会显示“状态不确定”。这类短信可能已经发出，系统不会把它当作普通失败快速重试，以免产生重复短信。

为避开 Air780 长短信接口在超规格输入下可能卡住的问题，主机和 Lua 都会在提交前拒绝超过 2048 个 UTF-8 字节的短信；16 KiB 串口帧上限覆盖 JSON 转义后的完整命令，Lua 侧也会隔离发送接口异常，避免一条异常短信终止后续发送队列。

所有设备必须同步烧录仓库中的 `main.lua` 1.3.0 或更高版本。旧脚本不会完整保存飞行模式来源或在设备端校验 ICCID，新版主机会将其标记为不可安全发送并拒绝发送请求。新版主机连接后也会主动协商开启接收 ACK。

未配置 `Devices` 时仍兼容旧版 `App.Serial.Port`，系统会创建 ID 为 `default` 的物理连接。旧数据库中没有 ICCID 快照的短信，以及换卡瞬间无法可靠确认身份的新接收短信，都会保留在“未分配短信”中，不会根据当前碰巧插入的卡自动归属；旧计划任务需要手动选择目标 SIM 后才能重新启用。

不同 Air780 型号、底板和固件对运行中热插拔 SIM 的检测能力不同。为了保证路由及时刷新，建议先断电换卡，再重新上电；如果确认当前硬件支持热插拔，也应等待 Web 中两张 SIM 都显示在新的模块上后再发送。

主要 API：

- `GET /api/serial/devices`：返回所有物理连接及其当前状态
- `GET /api/serial/sims`：返回所有已知 SIM（包括离线 SIM）及当前承载模块
- `GET /api/serial/status?simId=iccid:...`：查询指定 SIM 当前状态
- `POST /api/serial/sms`：请求体必须携带 `simId` 和 UUID 形式的 `requestId`（也可用 `Idempotency-Key` 请求头）。重试同一请求必须复用同一 ID；同 ID 且 `simId`/`to`/`content` 相同时只返回原 `messageId`、不会再次发送，参数不同则返回 409
- `POST /api/serial/flymode`：请求体必须携带 `simId`，运行时解析当前承载模块
- `POST /api/serial/reboot`：可按 `simId` 重启当前承载模块，也保留 `deviceId` 物理控制方式
- `POST /api/scheduled-tasks/:id/trigger`：请求体必须携带 UUID 形式的 `requestId`（也可用 `Idempotency-Key` 请求头）；浏览器响应丢失时必须复用原 ID，避免“立即执行”被重复下发

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


