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
- 支持钉钉、企业微信群机器人、企业微信应用、Bark、飞书、自定义 webhook、邮箱和 Telegram 通知
- 支持全局自定义入站短信转发正文，可附加接收手机号、发送方、接收时间及 SIM/设备信息
- 计划任务发送短信
- 同时管理多台 Air780，短信、状态和计划任务按 SIM 卡（ICCID）隔离
- 串口断线重连、接收 ACK/去重和发送结果超时保护

## 多设备配置

自动发现要求每台 Air780 烧录仓库中的 `main.lua` 1.4.0 或更高版本。正常情况下不需要逐台填写 ID、名称或串口，系统会周期发现所有通过项目专用握手的 Air780，并处理后续插拔：

```yaml
App:
  Serial:
    AutoDiscover: true
```

即使省略整个 `App.Serial`，也会默认自动发现。主机上还有其他 USB 串口设备时，可以增加可选白名单，避免探测白名单外的端口：

```yaml
App:
  Serial:
    AutoDiscover: true
    Ports:
      - "/dev/ttyUSB0"
      - "/dev/ttyUSB1"
```

`Ports` 只控制允许打开哪些物理端口，不决定数据属于哪张卡；Linux/macOS 下也可填写 `/dev/serial/by-id/...` 一类指向设备节点的稳定软链接。系统会为连接生成仅用于日志和诊断的内部 `deviceId`；普通用户界面不需要选择设备。

Windows 会通过系统串口信息识别 USB COM 口。如果驱动未提供完整的 USB 元数据，或只希望探测指定设备，可在“设备管理器 → 端口 (COM 和 LPT)”中确认端口名并配置白名单：

```yaml
App:
  Serial:
    AutoDiscover: true
    Ports:
      - "COM3"
      - "COM4"
```

界面优先用运营商返回的手机号码区分 SIM，并把已读取到的号码持久保存，SIM 离线时也不会退回串口名；没有号码的卡才显示 ICCID 尾号。真正的数据归属始终使用 ICCID，不依赖可能为空或变化的显示号码。

Docker 部署时还需要将所有串口映射到容器；自动发现只能看到已经映射进去的设备节点：

```yaml
devices:
  - /dev/ttyUSB0:/dev/ttyUSB0
  - /dev/ttyUSB1:/dev/ttyUSB1
```

系统从模块读取 ICCID，并生成稳定的 `simId`；Web 顶部按 SIM 切换，短信历史和计划任务都会固定保存目标 `simId`。把两张卡在两台 Air780 之间互换后，业务数据和计划任务仍跟随原 SIM，不会跟着串口或模块走。

身份字段的含义：

- **ICCID**：SIM 卡本身的固定编号，是短信和计划任务的业务主键。
- **IMSI**：运营商订阅身份，只作为辅助信息，不作为固定主键。
- **IMEI**：Air780 模块的硬件编号，只记录“这次由哪台模块处理”，不能代表 SIM 卡。

发送时主机会把目标 ICCID 下发给 Lua。Lua 会在入队和真正调用短信发送接口前再次读取并核对 ICCID；换卡、身份未确认或同一 ICCID 冲突时都会拒绝发送。已发现的 SIM 档案会保存在数据库里，所以 SIM 离线后仍可查看历史和编辑计划任务，但不能发送。

如果设备已把短信提交给基带、但最终回执时 SIM 身份发生变化或回执超时，记录会显示“状态不确定”。这类短信可能已经发出，系统不会把它当作普通失败快速重试，以免产生重复短信。

为避开 Air780 长短信接口在超规格输入下可能卡住的问题，主机和 Lua 都会在提交前拒绝超过 2048 个 UTF-8 字节的短信；16 KiB 串口帧上限覆盖 JSON 转义后的完整命令，Lua 侧也会隔离发送接口异常，避免一条异常短信终止后续发送队列。

自动发现使用 `main.lua` 1.4.0 新增的随机握手，只接受项目名和请求 ID 都精确匹配的完整协议帧，不会把普通 JSON 串口设备或历史残留数据误认为 Air780。主机正式连接后仍会重新读取 SIM 身份；探测回包不能直接建立短信路由。

旧版 `App.Serial.Port` 和 `App.Serial.Devices` 仍可继续使用；当 `Port` 非空或配置了 `Devices`，且未填写 `AutoDiscover` 时，系统保持静态模式。`main.lua` 1.3.x 仅能在明确指定串口的静态模式下继续使用。旧配置中的 ID/Name 仅用于兼容和诊断，也可省略并自动生成。不要把 `AutoDiscover: true` 与旧字段混用；需要限制自动探测范围时只使用 `Ports`。旧数据库中没有 ICCID 快照的短信，以及换卡瞬间无法可靠确认身份的新接收短信，都会保留在“未分配短信”中，不会根据当前碰巧插入的卡自动归属；旧计划任务需要手动选择目标 SIM 后才能重新启用。

如果自动模式没有发现设备，请依次确认：已烧录 1.4.0+ 脚本、当前用户有串口权限、Docker 已映射设备节点，以及 `Ports` 白名单与容器内路径一致。启用 debug 日志后可区分“无法枚举串口”“端口无法打开”和“未通过项目握手”。

不同 Air780 型号、底板和固件对运行中热插拔 SIM 的检测能力不同。为了保证路由及时刷新，建议先断电换卡，再重新上电；如果确认当前硬件支持热插拔，也应等待 Web 中两张 SIM 都显示在新的模块上后再发送。

## 通知渠道与转发格式

“通知渠道”页面除群机器人外，也支持企业微信自建应用（CorpID、AgentID、Secret、接收成员）和 Bark。企业微信应用可为获取 Access Token 与发送消息统一配置 HTTP/HTTPS/SOCKS5 代理；Bark 可使用 `https://api.day.app`，也可填写自建 Bark Server 地址。

通知渠道页面顶部直接展示“全局短信转发格式”，对所有已启用渠道生效，但不会改写数据库中保存的原始短信。模板支持以下变量：

- `{{content}}`：原始短信内容
- `{{receiver}}` / `{{to}}`：接收此短信的 SIM 手机号
- `{{from}}`：短信发送方
- `{{timestamp}}`：短信接收时间
- `{{device_name}}`、`{{sim_id}}`、`{{iccid}}`、`{{imsi}}`、`{{imei}}`：设备和 SIM 信息

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

#### Windows 原生运行

1. 从 Release 下载 `uart_sms_forwarder-windows-amd64.zip`（普通 Intel/AMD Windows 电脑）或 `uart_sms_forwarder-windows-arm64.zip`，解压到一个可写目录。
2. 编辑解压目录中的 `config.yaml`，至少修改管理员密码和 JWT 密钥。数据库及日志默认保存在同目录的 `data`、`logs` 文件夹中。
3. 用 USB 连接 Air780，并在设备管理器确认串口驱动已正常加载。
4. 在 PowerShell 中运行：

```powershell
Set-Location C:\path\to\uart_sms_forwarder-windows-amd64
powershell -ExecutionPolicy Bypass -File .\start.ps1
```

也可以直接运行程序，并通过 `-config` 指定任意位置的配置文件：

```powershell
.\uart_sms_forwarder.exe -config D:\uart-sms\config.yaml
```

程序会把配置文件所在目录作为工作目录，因此配置中的 `./data/app.db` 和 `./logs/sms.log` 在从快捷方式或其他目录启动时也能保持一致。启动后访问 [http://localhost:8080](http://localhost:8080)。如果其他电脑需要访问，请允许 Windows 防火墙中的 TCP 8080 入站连接。

从源码构建需要 Go 1.26、Node.js 24 和 npm：

```powershell
.\scripts\windows\build.ps1 -Architecture amd64
```

产物位于 `dist\uart_sms_forwarder-windows-amd64.zip`。

#### 自动打包与发布

仓库内置 GitHub Actions 发布流程。推送符合 `v1.2.3`（也支持 `v1.2.3-beta.1`）格式的标签后，会自动构建前端和多个平台的服务端，并创建 GitHub Release。也可以在 Actions 页面手动运行 `Release` 工作流并填写版本号。

```shell
git tag v1.0.0
git push origin v1.0.0
```

发布产物包含 Linux、Windows、macOS 和 FreeBSD 压缩包以及 `SHA256SUMS`；Windows 压缩包内附 `start.ps1`、`config.yaml` 和 `main.lua`。

#### docker 方式安装

```shell
# 创建空目录
mkdir /opt/uart_sms_forwarder
# 下载 docker-compose.yml 文件
wget https://raw.githubusercontent.com/HaloWww/uart_sms_forwarder/main/docker-compose.yml -O /opt/uart_sms_forwarder/docker-compose.yml
# 下载 config.example.yaml 文件
wget https://raw.githubusercontent.com/HaloWww/uart_sms_forwarder/main/config.example.yaml -O /opt/uart_sms_forwarder/config.yaml
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
wget https://github.com/HaloWww/uart_sms_forwarder/releases/latest/download/uart_sms_forwarder-linux-amd64.tar.gz
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
