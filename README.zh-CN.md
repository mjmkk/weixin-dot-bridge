# 微信与 dot 的原生消息桥

[English](README.md)

让个人微信连接到**已有 OpenAI dot 的原生消息房间**。在微信和 dot 原界面交替发送消息，继续使用同一个房间和原来的 dot。

```text
微信 ↔ 腾讯 iLink ↔ weixin-dot ↔ 已有 dot 的原生消息房间
```

桥是一个普通 Go 进程，只搬运消息和媒体原件。没有额外模型负责传话、改写、总结或转写；dot 继续使用它自己的工具和能力。

这是实验性社区项目。dot 接入采用从 Beeper 适配的 ChatGPT 未公开协议，不是 OpenAI 官方集成或承诺兼容的公开 API。能否使用取决于账号权限及两端服务的实际行为。

## 当前能力与边界

- 微信文字进入原 dot 房间；正常回复、进度和主动消息从同一房间读取并排入微信发送队列。
- 原 dot 界面的用户消息也会镜像到微信，标注说话方和原时间。微信刚转入且未改变的原消息不会回送；在原界面编辑、删除或改回原内容的变化会显示。
- 文件、图片和视频通过私有临时文件及固定工作缓冲区传输原件。桥不设固定文件总大小门槛；平台限制、磁盘空间、格式及网络超时仍然生效。
- 微信入站语音以**音频文件**交给 dot，由 dot 自己的工具读取。它不是原生语音对话界面，也没有实现出站语音气泡或自动音频转码。
- 私有 journal 保存游标、原 payload、稳定发送标识和结果，重启沿同一记录续接。POST 结果未知时保留记录，不盲目重发。

原生任务卡和审批仍需在 dot 原界面处理。编辑和删除以提示展示，不能撤回已经存在的微信气泡。原界面用户消息由 Bot 代转并标注身份，不会变成个人微信账号的右侧自发气泡。接口 ACK 不等于用户实收；微信发送还依赖可用的回复上下文。

## 构建

需要 Go 1.26 或更新版本及 Unix 主机。journal 使用 `flock`；可选服务模板适用于 Linux systemd。

```sh
git clone https://github.com/mjmkk/weixin-dot-bridge.git
cd weixin-dot-bridge
mkdir -p bin
go build -trimpath -o bin/weixin-dot ./cmd/weixin-dot
```

## 连接自己的账号

需要已有 dot 和自己本机上受支持的 ChatGPT/Codex 登录文件。默认读取 `~/.codex/auth.json`，可通过 `--auth` 指定其他私有文件。桥消费已有登录，不提供 ChatGPT 登录流程。凭据文件使用 0600，状态目录使用 0700。

1. 发现 dot，此命令不发消息：

   ```sh
   ./bin/weixin-dot discover --auth "$HOME/.codex/auth.json"
   ```

   将返回的 `room` 和 `dot` 保存在本机，用于启动桥。发送前会核对认证账号、发现的 dot 和房间成员。

2. 扫码绑定微信：

   ```sh
   ./bin/weixin-dot bind
   ```

   打开命令返回的本地二维码图片，在微信中扫码。成功后凭据保存在 `~/.config/weixin-dot/weixin.auth.json`。尚未完成的登录可用 `bind --resume` 续接；二维码过期需重新登录。程序不会自动覆盖已存在的凭据。

   微信要求验证码时，当前实现只通过私有 stdin 管道接收。Bash 中可这样续接已保存的二维码，验证码不会进入命令历史：

   ```bash
   read -r -s -p "WeChat verification code: " weixin_verify_code
   printf '\n'
   printf '%s\n' "$weixin_verify_code" | ./bin/weixin-dot bind --resume
   unset weixin_verify_code
   ```

3. 将占位符换成发现结果，启动桥：

   ```sh
   ./bin/weixin-dot bridge \
     --auth "$HOME/.codex/auth.json" \
     --room 'YOUR_VERIFIED_ROOM' \
     --dot 'YOUR_VERIFIED_DOT'
   ```

   在微信发送一条新消息，建立回复上下文。默认微信凭据路径为 `~/.config/weixin-dot/weixin.auth.json`，journal 为 `~/.local/state/weixin-dot/bridge.json`。可通过 `--weixin-auth`、`--state` 指定自己的私有位置。

用 Ctrl+C 停止后，使用**同一 journal**重新启动。一个 journal 同时只能由一个进程持有。账号或房间核验失败时，恢复原账号登录、确认失败原因后再启动；不要通过删除 journal 来重试未知发送或上传。未知上传没有取得可靠文件标识，因此保留原件。

## 历史补发

日常聊天以初次连接时的房间基线开始，之后会回查该基线以来的消息，不会自动导入连接前的完整历史。

已排入的历史默认暂停。明确需要补发时，在同一个 `bridge` 命令中增加 `--history-backfill`。历史保留原说话方和时间，不写回 dot、不重跑任务。实时消息优先；历史发送结果未知时，不换一个发送标识绕过。移除该参数即可暂停补发，队列仍保留。

`backfill inspect` 和 `backfill enqueue` 接收已完成的私有 `messages.jsonl` 导出及其配套 manifest、profile、room 元数据；先核验再本地排队。本仓库没有通用历史导出命令。输入参数可查看 `./bin/weixin-dot backfill inspect -h`。

## 可选 Linux 用户服务

将二进制安装到 `~/.local/bin/weixin-dot`，把 [deploy/weixin-dot.service](deploy/weixin-dot.service) 复制到 `~/.config/systemd/user/`，替换 `__VERIFIED_ROOM__`、`__VERIFIED_DOT__`。使用其他凭据或 journal 位置时，在模板中调整参数。

```sh
systemctl --user daemon-reload
systemctl --user enable --now weixin-dot.service
```

模板保留默认 journal，并限制文件权限。需要主机专用网络设置时，保存在自己本机的 `~/.config/weixin-dot/network.env`。

## 开发与来源

```sh
go test -race ./...
go vet ./...
```

测试使用本地夹具和模拟传输，不登录真实账号或发送消息。凭据、二维码、聊天归档、journal、媒体原件和签名 URL 应留在仓库外，也不要放进问题报告。

参见 [dot 客户端来源](docs/dot-source.md)、[微信协议来源](docs/weixin-source.md) 和 [第三方声明](THIRD_PARTY_NOTICES.md)。项目采用 [GNU AGPL v3](LICENSE)，第三方组件保留各自声明。适配的原生 dot 客户端版权为 Copyright (c) 2026 Beeper。
