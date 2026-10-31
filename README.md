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
- 可选 `direct` 独立转发：Air780 通过 SIM 卡流量发送通知，并统计转发请求产生的流量

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

## direct 独立转发模式

默认的 `uart` 模式保留现有行为：Air780 通过 USB 将短信交给 Ubuntu/Windows 服务，由上位机执行通知渠道配置。`direct` 模式则由 Air780 使用 SIM 卡自己的蜂窝数据连接发送通知，无需运行上位机。SIM 卡必须已开通短信和数据流量业务；建议先使用第一卡槽（SIM0）验证。

下载 Release 中的 `uart_sms_forwarder-lua.zip` 即可获得独立运行所需的脚本。用 LuaTools 同时烧录其中的 `main.lua`（1.6.0+）、`direct_forward.lua`、`forward_config.lua`，并添加默认 lib。为模组选择支持 `http`、`mobile`、`fskv` 的 LuatOS 固件；不能使用 AT 固件。1.5.x 脚本仅支持修改文件后烧录，若要通过 exe 配置并保存到板子，需要先升级一次这三个设备脚本。

### 从 Windows / Ubuntu 配置并保存到板子

1. 启动新版 exe 或 Ubuntu 服务，连接并选中目标 SIM，等待身份确认。
2. 打开“串口控制”页面的“板载独立转发”，选择“SIM 流量独立转发”，填写并启用 Bark、企业微信应用或 Webhook 渠道，可同时设置模板和 APN。
3. 点击“保存到板子”，等待设备持久保存并回读确认成功。只有收到设备确认后才显示保存成功；超时或报错时应重新读取设备配置，不能把电脑里修改过的表单视为已保存。
4. 关闭 exe / Ubuntu 服务，保持板子供电，用另一部手机发送测试短信。板子会自行联网并转发；断电后重新供电也会读取相同设置。

设置保存在当前物理板子的 Flash 中，不依赖 exe、浏览器或电脑数据库。多台板子需要分别选中并保存；把 SIM 移到另一块板子不会自动迁移通知设置。日后修改渠道或切换回“主机转发”都可在页面保存，无需再次烧录。退出程序不会关闭板载独立转发；要关闭时，应连接设备并保存“主机转发”模式。

板载配置最多包含三个渠道，编码后的配置总长不能超过 3800 字节；CA 证书和模板也计入该长度。Flash 不可用、写入失败、配置超限或 SIM 身份不匹配时会拒绝保存，不会只留在电脑上却提示“保存到板子成功”。渠道密钥仅在明确读取板载配置的认证接口中返回，不会附在日常状态轮询中。

模式和渠道保存后会立即应用。APN 在部分固件中要等下次移动数据连接激活才生效；如修改 APN 后仍未联网，可重启板子，让它在联网前加载已保存的 APN。

`forward_config.lua` 仍可填写出厂/脚本默认值，尚未保存板载配置时会使用它；已持久保存的板载配置优先于脚本默认值。例如，预配置独立模式的脚本可写为：

```lua
return {
    mode = "direct", -- 改回 "uart" 即恢复上位机转发
    apn = "",        -- 留空使用运营商默认 APN，专网卡按卡商要求填写
    queue_limit = 8,
    max_attempts = 3,
    retry_base_seconds = 30,
    retry_max_seconds = 900,
    min_interval_seconds = 2,
    stats_save_seconds = 60,
    timezone_offset_seconds = 8 * 3600,
    template = "{{content}}\n接收号码：{{receiver}}\n时间：{{time}}",
    channels = {
        {
            id = "bark",
            type = "bark",
            enabled = true,
            server = "https://api.day.app",
            key = "填写你的 Bark Key",
            title = "短信通知",
        },
        {
            id = "wecom",
            type = "wecom_app",
            enabled = false,
            corp_id = "",
            agent_id = 1000002,
            secret = "",
            to_user = "@all",
        },
        {
            id = "webhook",
            type = "webhook",
            enabled = false,
            url = "https://example.com/sms",
        },
    },
}
```

渠道的 `id` 应保持唯一且稳定，用于记录每条消息在该渠道的发送结果。当前独立模式支持 Bark、企业微信应用和 JSON Webhook，并转发短信与来电通知；上位机的其他渠道仍仅在 `uart` 模式使用。“通知渠道”页面中的主机渠道和全局包装配置，与“板载独立转发”分别保存，不会暗中互相覆盖。密钥会保存在设备脚本或板子 Flash 中，不要把填写真实密钥的文件提交到公开仓库；普通设备状态和日志不会返回板载渠道密钥。

独立模板支持 `{{content}}`、`{{from}}`、`{{receiver}}`/`{{to}}`、`{{iccid}}`、`{{imei}}`、`{{imsi}}`、`{{time}}`（按 `timezone_offset_seconds` 格式化）和 `{{timestamp}}`（Unix 秒）。卡内没有保存手机号时，`receiver`/`to` 可能为空。JSON Webhook 还会携带 `message_id`，并设置同值的 `Idempotency-Key` 请求头，服务端可据此去重；返回 HTTP 2xx 表示成功，推荐无正文的 `204` 响应以减少流量。企业微信应用的可信 IP 限制仍适用，SIM 的公网出口 IP 可能变化，直连失败时需先检查企业微信返回的配置错误。

联网请求强制使用蜂窝网卡，等待移动数据就绪后发送。只在收到通知或需要重试时访问网络，不发送用于保活或流量上报的 HTTP 心跳。企业微信 Access Token 按有效期缓存在 RAM 中，按需刷新；Bark 与 Webhook 直接发送精简 JSON。相邻渠道转发尝试至少间隔 `min_interval_seconds`，失败按指数退避、每条消息的每个渠道最多尝试 `max_attempts` 次，避免断网时不停请求。一次企业微信尝试可能包含取 Token 与发送消息两次 HTTP 请求。固件 HTTP 接口可能采用短连接，实际流量仍包含 DNS、TCP/TLS 握手、响应与重传开销，不能用短信字数估算。

队列默认最多保存 8 条通知。支持且可用的 `fskv` 会立即保存新消息和各渠道结果，重启后可继续处理待发送项；新队列记录编码后最多 3400 字节，为后续重试和完成状态预留空间，适应 `fskv` 的 4096 字节单值限制。队列满、记录过大或重试耗尽都会计入相应状态计数。`fskv` 不可用时会明确显示 `storage_available = false`，退化成 RAM 队列，断电会丢失待发内容。渠道已确认成功后不会因其他渠道失败而重发，但服务器已处理、模块却未收到响应等不确定情况仍可能造成重复通知。

`direct` 模式会拒绝上位机的自动飞行模式指令，保持可接收短信和联网的状态；用户手动开启飞行模式后会暂停独立转发，退出且恢复网络后再继续，脚本不会为转发自动退出飞行模式。队列有已确认的 ICCID 时，只允许由同一张活动 SIM 发送，避免换卡后让另一张卡支付旧消息流量。`direct` 模式下连接新版 Ubuntu 服务仍可保存 USB 接收到的短信历史，主机会跳过该消息的二次通知；没有主机时则不具备网页历史查询、网页配置或计划任务功能。

### 查看转发流量

连接上位机后，可在设备状态页查看独立转发状态、累计上行/下行流量、请求数以及队列和失败计数；纯独立运行时可通过串口发送 `get_forward_stats` 查看同一份 `direct_forward` 状态（`get_status` 也会返回该状态）：

```text
CMD_START:{"action":"get_forward_stats"}:CMD_END
```

流量来自每次 HTTP 请求前后 `mobile.dataTraffic()` 的差值，包括企业微信获取 Token、通知发送及失败重试。它是模块测得的“转发请求期间蜂窝流量估计”，不是 HTTP 正文字节数，也不等于运营商账单；若同时运行其他联网任务，这些任务的流量可能被一起计入。统计累计保存在模块内，换 SIM 不会清零，也不按 SIM 拆分。固件无法读取流量计数时会显示 `counter_available = false`，不能将此时的 0 当作没有流量消耗。

统计默认每 60 秒及队列状态变化时写入 `fskv`，兼顾掉电恢复和 Flash 写入次数；突然断电时最后尚未保存的一小段统计可能丢失。节省流量最直接的方法是只启用必需的渠道，保持 Token 缓存，使用简短模板，并避免过短的重试间隔。

`unmetered_requests` 会累计并保存计数缺失或清零期间无法测量的请求次数，即使后续计数恢复，仍能判断历史累计是否完整。固件计数接口参考 [mobile.dataTraffic](https://docs.openluat.com/osapi/core/mobile/)，蜂窝适配器及 CA 参数参考 [http.request](https://docs.openluat.com/osapi/core/http/)。接收方配置改变后，旧待发记录会标记失败而不会改投新接收方；HTTP 尝试检查点写入失败时仅本地等待存储恢复，避免因不断重启而反复消耗流量。

渠道只接受 HTTPS 地址。需要服务器证书链校验时，在渠道中填写 `ca_cert`（PEM 格式 CA 证书，也可用 `io.readFile("/luadb/server-ca.crt")` 读取并把证书一并烧录），并验证当前固件支持该参数。未显式配置 CA 时，不能仅凭 HTTPS 地址认定证书链已经通过校验。

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
- `GET /api/serial/forwarding?simId=iccid:...`：读取当前承载板子的转发模式、渠道、模板及持久保存状态，需要 `main.lua` 1.6.0+
- `PUT /api/serial/forwarding`：请求体 `{simId, config}`，通过串口写入板子并等待持久保存和回读确认，不仅保存到电脑
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

使用 [**LuaTools**](https://docs.openluat.com/air780epm/common/Luatools/) 烧录 `main.lua`、`direct_forward.lua` 和 `forward_config.lua` 三个脚本，并勾选“添加默认 lib”。第一次烧录需要点击「下载底层和脚本」。默认 `mode = "uart"`，继续由 Ubuntu/Windows 上位机转发；独立运行的配置见下文 `direct` 模式说明。

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

程序启动时会自动创建配置中的 SQLite 文件父目录和日志目录，直接运行 exe 也不需要预先执行启动脚本。运行目录需要可写；若在映射盘或网络共享上遇到 SQLite 无法打开文件，请先在本地磁盘目录验证。Windows 脚本兼容系统自带的 Windows PowerShell 5.1。

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

发布产物包含 Linux、Windows、macOS 和 FreeBSD 压缩包、纯设备脚本包 `uart_sms_forwarder-lua.zip` 以及 `SHA256SUMS`。所有平台压缩包内附 `config.yaml` 和三个设备脚本：`main.lua`、`direct_forward.lua`、`forward_config.lua`；Windows 包另附 `start.ps1`。独立运行只需下载纯脚本包。

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

自动安装并启动 systemd 服务（需要 root 权限）：

```shell
sudo /opt/uart_sms_forwarder/uart_sms_forwarder \
  -install-service \
  -config /opt/uart_sms_forwarder/config.yaml
```

该命令会创建默认的 `data`、`logs` 目录，生成 `/etc/systemd/system/uart_sms_forwarder.service`，执行 `systemctl daemon-reload`，设置开机启动并立即启动服务。重复执行可更新已有服务配置；如果在 `config.yaml` 中使用了其他数据或日志目录，请提前创建相应目录。

查看服务状态和日志：

```bash
systemctl status uart_sms_forwarder.service
journalctl -u uart_sms_forwarder.service -f
```

打开浏览器访问 8080 端口。

修改密码等配置项，请参考 [config.example.yaml](config.example.yaml) 文件。
