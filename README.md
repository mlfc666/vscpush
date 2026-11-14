# vscpush

一条命令完成 **VS Code Server 的本地高速下载 + 推送到远程服务器（可选直接装好）**。

起因是 VSCode Remote-SSH 在服务端自下载 server 时，走的是单连接，常见只有几百 KB/s，一个
225 MB 的包要 8~10 分钟，表现就像「卡在 Setting up server」。手工流程（浏览器下载 → scp →
tar 解包）又太啰嗦，于是把它做成一个自包含的 Go CLI。

名字：`vsc`（VS Code Server）+ `push`。

## 特性

- **零外部依赖**：不调用 aria2c / curl / scp / rsync，下载、上传、校验全在进程内。
- **并发 Range 下载**：默认 16 连接。CDN 按连接限速时，实测从 445 KB/s 提到 7 MiB/s。
- **SSH/SFTP 上传**：支持 ssh-agent、私钥（`-key`）、密码（`VSCPUSH_PASSWORD`）。
- **端到端校验**：本地算 sha256，上传后在远端用 `sha256sum` 复算比对，坏包立刻失败。
- **架构自动识别**：登录后 `uname -m` → `arm64` / `x64` / `armhf`，不用猜。
- **可选一键安装**：`-install` 解包到 `~/.vscode-server/bin/<commit>`；若该 commit 的 server
  正在运行会**拒绝覆盖**（保护在线会话），除非显式 `-force`。
- 进度条、ETA、限速统计；非 TTY 自动降级为低频文本行。

## 构建

```bash
go build -trimpath -ldflags "-s -w" -o vscpush .
# 或
go install vscpush@latest
```

依赖只有 `github.com/pkg/sftp` 和 `golang.org/x/crypto`。

装到 PATH 里，之后任意目录可调用：

```bash
go build -trimpath -ldflags "-s -w" -o vscpush .
install -m 0755 vscpush ~/.local/bin/vscpush   # 前提：~/.local/bin 在 $PATH 中
which vscpush && vscpush -version
```

## 用法

三个必填参数：`-commit`、`-host`、`-user`。

```bash
# 下载 + 上传到远端主目录
./vscpush -commit 69047191fc427f2f92828279aae1d90d8eeda7ab \
          -host 10.24.10.224 -user liaobin

# 顺手装好，装完打印 node 版本
./vscpush -commit 69047191fc427f2f92828279aae1d90d8eeda7ab \
          -host 10.24.10.224 -user liaobin --install

# 只看计划，不下载不上传
./vscpush -commit 69047191fc427f2f92828279aae1d90d8eeda7ab \
          -host 10.24.10.224 -user liaobin -dry-run
```

### 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-commit` | 必填 | VS Code 日志里的 40 位 commit id |
| `-host` | 必填 | 目标服务器 IP 或域名 |
| `-user` | 必填 | 目标服务器 SSH 用户 |
| `-port` | `22` | SSH 端口 |
| `-arch` | `auto` | `auto` / `x64` / `arm64` / `armhf`；auto 由 `uname -m` 推断 |
| `-conns` | `16` | 下载并发连接数（上限 32） |
| `-key` | 空 | 私钥路径；空则依次尝试 `~/.ssh/id_ed25519`、`id_ecdsa`、`id_rsa` 与 ssh-agent |
| `-password` | 空 | SSH 密码，建议改用环境变量 `VSCPUSH_PASSWORD` |
| `-remote-dir` | 家目录 | 远端落地目录（支持 `~` 与相对路径） |
| `-remote-name` | `vscode-server-linux-<arch>.tar.gz` | 远端文件名 |
| `-local-dir` | `$TMPDIR` | 本地下载目录 |
| `-install` | `false` | 上传后解包安装 |
| `-install-dir` | `~/.vscode-server/bin/<commit>` | 安装目录 |
| `-force` | `false` | 该 commit 的 server 在跑也强制覆盖 |
| `-dry-run` | `false` | 只探测并打印计划 |
| `-quiet` | `false` | 关闭进度条 |
| `-no-verify` | `false` | 跳过远端 sha256 复算 |
| `-strict-host-key` | `false` | 主机密钥不在 known_hosts 里就拒绝（默认 accept-new） |
| `-timeout` | `30m` | 整体超时 |

## 实测数据（本机 → aarch64 服务器，225.8 MiB）

| 阶段 | 吞吐 | 耗时 |
|---|---|---|
| 下载（16 连接） | 7.0 MiB/s | 32s |
| 上传（SFTP） | 8.2 MiB/s | 27s |
| 合计 | — | **1m01s** |

对照：服务器上单连接 `curl` 是 445 KB/s / 8m39s；`aria2c + scp` 方案约 55s（下载 34.6s +
上传 20.5s，scp 单流略快于原生 SFTP）。

## 实现要点

1. **探测**：发一个 `Range: bytes=0-0` 的 GET，既跟随 302 拿到最终地址，也确认服务端返回
   206（支持分片）并从 `Content-Range` 得到总长度。
2. **并发下载**：按 `conns` 把文件切段，每段独立 goroutine + 最多 8 次退避重试，用
   `WriteAt` 写同一文件的不同偏移；进度只做原子累加，渲染交给单个 ticker。
3. **校验**：流式算 sha256。
4. **上传**：`sftp.MkdirAll` + `Create` + `io.CopyBuffer`，`UseConcurrentWrites` 打开。
5. **远端校验**：`sha256sum` 输出与本地比对。
6. **安装**：`pgrep -f bin/<commit>/out/server-main.js` 做护栏 → `mkdir -p` →
   `tar -xzf ... --strip-components=1` → `node --version` 自检。

## 代码结构

| 文件 | 职责 |
|---|---|
| `main.go` | 参数解析、帮助、入口 |
| `run.go` | 流水线编排与结果汇总 |
| `download.go` | 探测、单流/并发下载、sha256 |
| `remote.go` | SSH 认证、主机密钥、SFTP 上传、远端命令、安装 |
| `progress.go` | 进度条、单位与耗时格式化 |
| `cli_test.go` | 纯函数单元测试 |

## 限制

- 只覆盖 `x64` / `arm64` / `armhf` 的 linux 包，不处理 alpine 变体。
- 上传是单流 SFTP，约 8 MiB/s；要更快可以扩展成多连接分片上传。
- 不支持断点续传：每次覆盖下载；重试只发生在单个分片内部。
- 带口令的私钥不做交互式解密，请用 ssh-agent 或 `ssh-keygen -p` 去掉口令。
- `-password` 会留在 shell 历史里，优先用 `VSCPUSH_PASSWORD`。

## 已知边界

- **默认拒绝覆盖运行中的 server**：`-install` 命中 `pgrep` 时会报错退出（退出码 1）并提示
  `-force`，避免把正在使用的会话拆掉。
- 下载走 `HTTPS_PROXY` / `HTTP_PROXY`（`http.ProxyFromEnvironment`），内网代理环境可用。
- 首次连接未知主机时打印密钥指纹后继续（accept-new 语义），可用 `-strict-host-key` 收紧。
