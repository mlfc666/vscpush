# vscpush

一条命令完成 **VS Code Server 的本地高速下载 + 推送到远程服务器（可选直接装好）**。

起因是 VSCode Remote-SSH 在服务端自下载 server 时，走的是单连接，常见只有几百 KB/s，一个
225 MB 的包要 8~10 分钟，表现就像「卡在 Setting up server」。手工流程（浏览器下载 → scp →
tar 解包）又太啰嗦，于是把它做成一个自包含的 Go CLI。

名字：`vsc`（VS Code Server）+ `push`。

## 特性

- **下载默认交给 aria2c**：系统装了 aria2c 就用它（`-engine auto`，其终端读数就是进度条），
  没装则自动回退到内置的并发 Range 下载器；上传与校验不依赖 scp/rsync，全在进程内。
- **内置并发 Range 下载器**（`-engine builtin`）：16 连接、1 MiB 动态分片（worker 抢占），
  链路不均匀时不会被最慢的那条连接拖住；CDN 按连接限速时实测从 445 KB/s 提到 7~10 MiB/s。
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
| `-no-upload` | `false` | 只下载，不上传（配 `-arch` 可完全不连远端） |
| `-url` | 空 | 覆盖下载地址，用于自建镜像 / 离线缓存 |
| `-engine` | `auto` | 下载引擎：`auto`（有 aria2c 就用）/ `aria2c`（强制）/ `builtin`（内置） |
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

## 基准测试

同一份 225.8 MiB 的 arm64 包，与 `aria2c` **交替执行**（避免链路波动只偏袒一方）。

**真实 CDN（update.code.visualstudio.com）**

| 连接数 | aria2c 中位数 | vscpush 中位数 |
|---|---|---|
| 16 | 32.09s（7.04 MiB/s） | 30.85s（7.32 MiB/s） |
| 32 | 22.44s（10.06 MiB/s） | 21.88s（10.32 MiB/s） |

**可控不均衡链路**：本地服务器按 TCP 连接限速（8 条 10 MiB/s + 8 条 1 MiB/s），80 MiB，16 连接

| 实现 | aria2c | vscpush |
|---|---|---|
| 等分固定块（v0.1） | 1.93s | 4.94s（慢 2.6×，被最慢连接拖住） |
| 动态分片（v0.2 起） | 1.90s | 1.94s |

> 上表中 vscpush 的成绩是**内置下载器**（`-engine builtin`）的实测；v0.3 起下载默认交给
> aria2c，此时下载阶段的表现与直接跑 aria2c 等价。

结论：CDN 是「每连接统一限速」，调度策略的差异落在噪声里，两者持平；真正拉开差距的是
链路不均衡——这也是把等分固定块换成动态分片的原因。

aria2c 明显更强的地方是**功能**而非速度：断点续传、多镜像/Metalink/BitTorrent、RPC、
内置校验、整体限速等。本项目刻意只覆盖「一条固定链路：下载官方包 → 推到一台机器」。

## 实现要点

1. **探测**：发一个 `Range: bytes=0-0` 的 GET，既跟随 302 拿到最终地址，也确认服务端返回
   206（支持分片）并从 `Content-Range` 得到总长度。
2. **并发下载**：整包按 1 MiB 切片进队列，`conns` 个 worker 竞争领取（动态分片），
   每片独立重试最多 8 次，用 `WriteAt` 写同一文件的不同偏移；进度只做原子累加，
   渲染交给单个 ticker。
3. **校验**：流式算 sha256。
4. **上传**：`sftp.MkdirAll` + `Create` + `io.CopyBuffer`，`UseConcurrentWrites` 打开。
5. **远端校验**：`sha256sum` 输出与本地比对。
6. **引擎选择**：`exec.LookPath("aria2c")` 决定 `-engine auto` 走哪条路；aria2c 的实时读数
   在管道下是块缓冲的，解析它做进度条只会「长时间 0% 然后瞬间 100%」，所以 aria2c 引擎直接把
   stdout/stderr 交给终端（真 TTY 下它会原地刷新），失败时自动回退内置下载器重来一遍。
7. **安装**：`pgrep -f bin/<commit>/out/server-main.js` 做护栏 → `mkdir -p` →
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
- aria2c 是**可选**依赖：装了更快更稳，没装则用内置下载器（功能等价，仅调度策略不同）。
- 上传是单流 SFTP，约 8 MiB/s；要更快可以扩展成多连接分片上传。
- 不支持断点续传：每次覆盖下载；重试只发生在单个分片内部。
- 带口令的私钥不做交互式解密，请用 ssh-agent 或 `ssh-keygen -p` 去掉口令。
- `-password` 会留在 shell 历史里，优先用 `VSCPUSH_PASSWORD`。

## 已知边界

- **默认拒绝覆盖运行中的 server**：`-install` 命中 `pgrep` 时会报错退出（退出码 1）并提示
  `-force`，避免把正在使用的会话拆掉。
- 下载走 `HTTPS_PROXY` / `HTTP_PROXY`（`http.ProxyFromEnvironment`），内网代理环境可用。
- 首次连接未知主机时打印密钥指纹后继续（accept-new 语义），可用 `-strict-host-key` 收紧。
- **主机密钥算法协商**：Go 的默认顺序先试 `ecdsa-sha2-nistp256`，而 OpenSSH 客户端优先
  `ssh-ed25519`。若 `known_hosts` 里只记了 ed25519（平时都用 `ssh` 连就会这样），Go 会拿
  ecdsa 去比对并报 `knownhosts: key mismatch`——看着像服务器换了密钥，其实只是算法不同。
  v0.3.1 起会先读 `known_hosts` 里该主机记录过的类型并提到协商顺序最前，与 OpenSSH 行为一致。
