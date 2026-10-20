# tele-admin

Telegram 群组管理 Bot（首个垂直切片）。

## 代码归属

- 工作区无 git 历史，无法追溯提交记录与作者。
- 现有代码作者**未知**，已记录为未知并由开发工程师接手检查、修复与补全。
- 本切片在现有代码基础上完成，未改变模块路径 `github.com/viceor/tele-admin`。

## 功能范围（首个垂直切片）

- Go 服务支持 `polling`（long polling）与 `webhook` 两种运行模式。
- 命令：`/start`、`/help`、`/welcome`、`/lang`、`/tz`。
- 群组基础治理：
  - 新成员欢迎消息（可按成员去重，可自定义文案，跳过 bot）。
  - 关键词拦截（大小写不敏感，命中后删除消息并提示）。
  - 最小反垃圾规则链（每分钟消息上限 + 最小发送间隔）。
- 严格校验 Telegram 管理员权限：`getChatMember` 返回 `creator` 或 `administrator` 才允许执行管理命令；配置在 `admin.owner_ids` 中的 Bot 属主始终拥有管理权限（先于 `getChatMember` 判定，API 失败也不锁定属主）；其余用户权限校验失败时按拒绝处理。
- 群/频道配置支持语言（`en`、`zh`）与 IANA 时区。

## 环境要求

- Go 1.27.2（见 `go.mod`）。
- 依赖：`github.com/go-telegram/bot v1.27.0`、`gopkg.in/yaml.v3 v3.0.1`。

## 配置

复制 `config.example.yaml` 为 `config.yaml`，并填入真实 bot token：

```yaml
token: "123456:REPLACE_WITH_YOUR_BOT_TOKEN"
mode: polling            # polling | webhook
```

关键配置项：

- `mode`：`polling` 或 `webhook`。
- `webhook.listen` / `webhook.path` / `webhook.public_url` / `webhook.secret_token`：webhook 模式使用。`public_url` 必须为 HTTPS，`Start` 会用它调用 Telegram `setWebhook` 并携带 `secret_token`。
- `api_server_url`：可选，指向模拟 Telegram API 服务器（测试用）。
- `log_level`：日志级别，取值 `debug`、`info`、`warn`、`error`（默认 `info`）；已生效。
- `db.path`：持久化文件路径。配置后 chat 语言/时区/欢迎状态、AI 名片、发布目标注册表与发布记录，以及 `/clone` 使用的 profiles 和 templates 表，均写入该文件并可跨重启恢复；未配置时上述数据仅存内存，进程重启后丢失。反垃圾 rate 窗口与 in-flight 欢迎占位是瞬态状态，不落盘（配置样例已注明）。
- `i18n.default_language` / `i18n.supported`：语言设置。`default_language` 会作为新 chat 的默认语言。
- `timezone.default`：默认 IANA 时区，会作为新 chat 的默认时区。
- `admin.enforce_admin` / `admin.owner_ids`：管理员校验开关与 owner 白名单。
- `spam.max_messages_per_minute` / `spam.min_message_interval_ms`：反垃圾阈值。
- `welcome.enabled` / `welcome.message`：欢迎消息。
- `keywords.blocked`：关键词拦截列表。

示例文件不包含任何真实凭据。

## 安全边界（MYC-32 修复后）

以下 fail-closed 约束在配置加载（`config.Validate`）与启动（`bot.New`）两处强制执行：

- **Webhook 鉴权（H-1）**：`mode: webhook` 必须配置非空 `webhook.secret_token`；该值通过 `WithWebhookSecretToken` 接入客户端，入站更新必须携带匹配的 `X-Telegram-Bot-Api-Secret-Token` 头，否则不进入业务 handler。启动时 `Start` 先成功绑定 `webhook.listen`，再用 `webhook.public_url`（必须为 HTTPS）调用 Telegram `setWebhook` 并携带同一 `secret_token`；绑定失败时不会调用 `setWebhook`（远端注册保持不变），`setWebhook` 失败时关闭已绑定的 listener 并让 `Start` 返回错误，不留下运行中的 listener。
- **Webhook 监听地址（H-1）**：`webhook.listen` 仅接受 loopback、私有或 link-local 地址（如 `127.0.0.1`、`10.x`、`192.168.x`、`::1`、`fd00::/8`）。本服务为明文 HTTP，因此拒绝全接口（`0.0.0.0`、`::`、空主机）、公网 IP 地址以及 hostname：这些必须改用真实 TLS 终止（将 TLS 终止放在前面并让本服务监听回环地址）。监听地址绑定失败（例如端口被占用）同样让 `Start` 返回错误，而不是只记录日志后继续等待。
- **管理员强制（M-1）**：`admin.enforce_admin` 必须显式为 `true`。省略或设为 `false` 时启动被拒绝，除非设置显式开发模式环境变量 `TELEADMIN_DEV_MODE=1`（仅用于本地开发，启动时打印显著安全警告）。
- **库日志与 debug（MYC-83）**：库的默认错误与更新输出经结构化 logger 脱敏：错误只记录操作标签、固定类别与数值字段（`retry_after`、`migrate_to_chat_id`），不记录错误文本或原始载荷；未匹配更新只记录 `update_id`。服务从不启用 `tgbot.WithDebug`，`log_level: debug` 不会转存请求或 webhook 报文（由 `library_log_test.go` 固定）。
- **匿名消息（M-2）**：所有命令 handler 对 `Message.From` 为 nil 的匿名消息（如频道代发）安全拒绝：不 panic、不执行命令，按无管理员权限处理。
- **owner 语义（M-3）**：`admin.owner_ids` 为 Bot 属主白名单。 enforce_admin 开启时，属主始终拥有管理权限（先于 `getChatMember` 判定，API 失败也不锁定属主）；非属主用户必须通过群管理员/群主校验。该语义由 `internal/governance` 与 `internal/bot` 测试固定。
- **Store 并发安全（M-4）**：`Store.Settings` 返回 `SettingsSnapshot` 值拷贝（含 `Welcomed` map 的深拷贝），调用方在锁外只能读取快照，无法改写内部可变状态；`Language`/`Timezone` 提供加锁只读访问器，所有 language/timezone/welcomed 读写均在锁内完成。`rates` map 在超过 `maxRateWindow`（默认 4096）时清理已失效的窗口，保留期取 `max(1 分钟, 2 * min_message_interval)`，因此当最小发送间隔超过 1 分钟时，min_interval 规则所需的上一时间戳不会被清理，内存仍与活跃用户数成正比。

## 运行

### 本地运行

```bash
go build ./...
go run ./cmd/teleadmin -config config.yaml
```

- `polling` 模式直接启动 long polling。
- `webhook` 模式启动时先绑定 `webhook.listen`，再调用 Telegram `setWebhook`（使用 `webhook.public_url` 与 `secret_token`）；绑定失败不会调用 `setWebhook`，`setWebhook` 失败会关闭已绑定的 listener，两种情况都让进程以错误退出。

### Docker Compose

需要预先安装 Docker Engine 与 Docker Compose plugin。首次启动时复制配置样例，并在 `config.yaml` 中填入真实 Bot token；生产使用时保持 `admin.enforce_admin: true`，并按需配置 `admin.owner_ids`。不要将包含真实 token 的 `config.yaml` 提交到版本控制。

```bash
cp config.example.yaml config.yaml
# 编辑 config.yaml，填入 Bot token
docker compose up --build -d
```

查看运行日志、停止服务：

```bash
docker compose logs -f teleadmin
docker compose down
```

Compose 将 `config.yaml` 以只读方式挂载，并把数据库保存到名为 `teleadmin-data` 的 Docker volume；普通 `docker compose down` 不会删除数据。要清除数据时执行 `docker compose down -v`。配置变更后执行 `docker compose up -d` 以重建容器。`config.yaml` 和 `data/` 已加入 `.gitignore` 与 `.dockerignore`，避免提交或打包真实凭据及本地数据。

## 测试（无需真实 token）

```bash
go test ./...
```

- `internal/bot` 内的测试使用 `net/http/httptest` 提供模拟 Telegram API 服务器，覆盖端到端路径（命令、治理、权限边界、错误路径）。
- 服务可在无真实 token 时通过模拟 Telegram API 场景启动：构造 `Config` 时设置 `api_server_url` 指向模拟服务器即可，`New` 不会因缺少真实凭据而失败。

## 已知限制

- 无有效 token 且未配置 `api_server_url` 时，`GetMe` 调用会失败并记录警告，服务仍继续启动；接入真实 Bot API 需要有效 token。
- 未配置 `db.path` 时 chat 设置、名片、发布注册表与发布记录，以及 `/clone` 使用的 profiles 和 templates 表，仅保存在内存中，进程重启后丢失；配置后这些数据可跨重启恢复（文件锁保证单写者）。反垃圾 rate 窗口与 in-flight 欢迎占位属于瞬态，刻意不持久化。
- 崩溃恢复的覆盖范围见上节：进程级 SIGKILL 已测试，断电与内核崩溃未测试。
- `go test -race ./...` 在本工作区**未运行**：环境无 C 编译器（`gcc` 缺失，CGO 关闭），`-race` 无法构建。须在具备 gcc 的 CI 中执行，结果在此之前不得报告为通过。
- 存储层硬化（MYC-72）：单写者锁改持在独立的 `<db.path>.lock` 边车文件上（不再随提交 rename 失效；该文件保留在磁盘上属正常现象）；数据库文件为带 `schema_version` 的版本化 envelope（`{"schema_version":1,"data":{...}}`），旧版裸 map 格式可照常加载并在下次提交时自动升级，未知 schema 版本 fail-closed 拒绝加载；加载时校验记录结构与主键一致性（null/非对象记录、主键与键不一致的数据库拒绝加载）；提交使用同目录随机名 + O_EXCL 临时文件（0600、校验普通文件、失败即清理），固定 `.tmp` 名不再存在；数据库/锁文件权限过宽时自动收紧为 0600，目录对 group/other 可写时拒绝启动。
- `welcomed` 集合按“曾欢迎过的成员”累积，当前不做淘汰：淘汰条目会对仍在群内的成员重复发送欢迎消息，属于可见行为变更。有界后续方案：处理 `left_chat_member` 更新，在成员离开时删除对应条目，使集合上界等于当前群成员数（或改为带 TTL 的持久化存储）。
- 欢迎状态仅在 Telegram 发送成功后记录：发送失败不会永久抑制后续重试，稍后的 join 事件会再次尝试；同一成员的并发/重复 join 事件通过原子占位只发送一次欢迎。上述“不做淘汰”的限制仍然成立。
- `/start` 与 `/help` 当前返回同一帮助文本。

## 依赖与后续

- 技术选型：研究员已确认（MYC-27）。
- 独立代码审查（MYC-30）、测试覆盖验收（MYC-28）、安全检查（MYC-29）。
- 安全修复：H-1 / M-1 / M-2 / M-3 已修复并补测试（MYC-32）；M-4 的 Store 锁外可变指针与 rate map 无界增长已修复并补快照/清理回归测试（MYC-36），`welcomed` 无界增长按上述已知限制保留有界后续方案。
- MVP 最终复审修复（MYC-40）：webhook 远端 `setWebhook` 注册与启动失败传播、监听地址仅限回环/私有、`i18n.default_language`/`timezone.default` 生效、rate-window 保留期覆盖 `2 * min_message_interval`、欢迎发送失败可重试，并同步文档与配置样例。
- 后续切片预留：文本/媒体多目标发布、AI 名片 CRUD、规则/模板克隆。

## 成员管理切片（MYC-46 切片 1，已实现）

- 命令：`/restrict`、`/unrestrict`。目标取自被回复消息的发送者，或命令参数中的数字用户 ID（`/restrict 42`）。不解析 `@username`，因为 Bot API 无法可靠地按用户名查找任意成员。
- 调用 Telegram `restrictChatMember`：`/restrict` 全部权限置为拒绝；`/unrestrict` 全部权限置为允许。限制状态保存在 Telegram 侧，本切片无本地持久化。
- 权限边界（fail-closed）：
  - 调用者必须通过 `requireAdmin`（Bot 属主、群主或管理员）。匿名消息与 `getChatMember` 失败均拒绝。
  - 目标若为 Bot 属主、群主或管理员，拒绝执行。目标状态查询失败时同样拒绝（视为无法证明目标可被操作）。
  - 仅当 `restrictChatMember` 返回 `ok=true` 且无错误时报告成功；任何错误或 `ok=false` 均报告失败。
- 自动测试：`internal/bot/members_test.go`（成功路径、非管理员/匿名/查询失败拒绝、受保护目标拒绝、API 错误与 `ok=false` 失败路径、参数校验、中文文案）。

## AI 名片切片（S3，已实现）

- 命令（`/card` 子命令族，任何群成员可用）：
  - `/card create <key=value>...`：创建自己的名片。必填 `name`；可选 `title`、`skills`、`contact`、`bio`。值可以含空格（`title=Senior Engineer` 或 `bio=hello` `world`）。
  - `/card show [user_id]`：展示自己的名片，或按数字用户 ID 查看他人名片。
  - `/card search <keyword>`：在本群名片中按关键字搜索（大小写不敏感，匹配 name/title/skills/contact/bio）。
  - `/card edit [user_id] <key=value>...`：编辑名片，**仅名片所有者**；非所有者编辑被拒绝并收到明确消息。省略未提供的字段（局部更新）。
- 字段与长度上限（按 rune 计）：`name` 1–64（创建时必填）、`title` 0–128、`skills` 0–256、`contact` 0–128、`bio` 0–512。空值、超长、未知字段、游离参数均返回明确错误；未知关键字参数返回用法。
- 存储：名片表按（chat、用户）唯一，保存字段与创建/更新时间；通过 P0 的 FileStore 落盘（`cards` 表，键 `<chat>_<user>`），配置 `db.path` 后重启保留；重复创建返回“已存在”且不覆盖原卡；编辑不存在的名片返回“未找到”。
- 权限边界（fail-closed）：匿名消息（无发送者）无法使用名片命令；非所有者编辑被拒绝；`/card show` 只接受数字用户 ID（不解析 `@username`，与成员管理切片一致）。
- 本切片**不调用任何外部模型服务**：名片内容全部由用户输入。AI 生成文本需先经 CTO 批准。
- 删除与退群清理（MYC-83 评估，结论：本切片不实现）：当前无 `/card delete` 命令，名片不随成员退群而删除。产品决定：名片是 chat 内的自我声明，成员退群后名片保留，以便已退群成员的历史信息可查；群管理员如需清理，可在后续切片中补充。数据处理影响：名片中的 contact 字段（用户自填）在本群内可被任何群成员通过 `/card show` 与 `/card search` 读取，不做按角色的字段级隐藏。若需删除或收窄可见性，须单独立项，并同时定义数据保留期与删除审计。
- 自动测试：`internal/bot/cards_test.go`（创建/展示/查询/编辑成功路径、重复创建、字段校验、越权拒绝、匿名拒绝、中文文案、跨重启持久化）与 `internal/store/cards_test.go`（CRUD、唯一性、按 chat 隔离、大小写不敏感搜索、持久化往返、时间戳）。

## 文本/媒体多目标发布切片（S2，已实现）

- 命令：
  - `/publish text <targets> <text>`：向目标群发送文本（文本可含空格）。
  - `/publish photo|video|document <targets> <file_id|URL> [caption]`：向目标群发送媒体，媒体按 Telegram `file_id` 或 `http(s)` URL 提供；本地路径（含路径分隔符、盘符、以 `.` 开头）一律拒绝，Bot 从不读取本地文件。
  - `/targets`：`targets` 为逗号分隔的已注册 chat ID（可为负数，如超级群/频道），或 `all` 表示全部已注册目标。
  - `/publish_add`：把当前 chat 注册为发布目标；仅限该 chat 的群管理员或 Bot 属主。私聊不能作为发布目标。
  - `/publish_remove`：取消当前 chat 的注册；仅限该 chat 的群管理员或 Bot 属主。
- 权限边界（fail-closed）：
  - `/publish` 仅限 Bot 属主或群主/管理员（`requireAdmin`）；匿名消息按无权限拒绝。
  - `/publish` 拒绝私聊来源（配置属主除外）：私聊参与者会被 Bot API 解析为 chat 创建者，若无此拦截任何用户都能经私聊向全部注册目标广播。
  - 显式指定的每个目标都必须已注册（`publish_add` 注册），否则整条命令在任何发送前被拒绝。
  - 逐目标校验 Bot 自身的发送权限（`getChatMember(bot)`：creator/administrator 直接允许；restricted 检查 `is_member`+`can_send_messages`；普通 member 追加 `getChat` 默认权限检查；查询失败视为无权限）。
  - 逐目标返回结果；部分失败时摘要明确标注“存在失败”，**不**报告整体成功；仅当全部目标成功时报告成功。
- 限制：目标数（`publish.max_targets`，默认 10）与内容长度（`publish.max_content_length`，默认 4096）超限即拒绝；媒体说明额外受 Telegram 1024 字符上限约束。
- 记录：每次发布写入 store（发布任务 + 逐目标结果 + 时间戳，ID 单调递增），配置 `db.path` 后重启保留；目标注册表同样持久化。
- 自动测试：`internal/bot/publish_test.go`（四类内容成功、file_id 与 URL、部分失败不报整体成功、Bot 无发送权限/查询失败 fail-closed、非管理员/匿名拒绝、属主放行、未注册与无目标拒绝、超限拒绝、本地路径拒绝、用法错误、私聊拒绝、中文文案、发布记录）与 `internal/store/publish_test.go`（注册表 CRUD、重复注册/移除未注册、持久化往返、ID 跨重启延续、并发访问）。

## 配置/规则/模板克隆切片（S4，已实现）

- 命令（均要求管理员权限，fail-closed）：
  - `/clone <source_chat_id>`：把源 chat 的 Bot 配置（语言、时区、欢迎语）与规则（关键词黑名单）复制到本 chat。**源 chat 与目标 chat 双方都必须**由 Bot 属主/群主/管理员授权；任一侧校验失败（含查询失败）即拒绝且不写入。
  - `/template save <name>`：把本 chat 当前生效的配置与规则保存为命名模板（未配置过的 chat 使用全局默认值作为“生效配置”，同样可克隆）。模板属主为本次保存的 chat：每个 chat 拥有自己的命名空间，其他 chat 用同名 save 会在它自己的命名空间内创建/覆盖自己的模板，且不会触碰原属主的记录（私聊与群/频道 chat ID，含负数，均可持有模板；仅属主 0 视为无属主而拒绝）。
  - `/template apply <name>`：把模板应用到本 chat；模板结构校验发生在写入之前，非法模板拒绝且不做任何部分写入。
  - `/template export <name>`：以带版本号的 JSON 包（`{"format":1,...}`）导出模板；导出写入审计日志——成功导出记录 chat、name、actor、version，被拒（名称不属本群）记录 chat、name、actor，不带 version。
  - `/template import <name> <JSON>`：导入带版本号的 JSON 包；**未知格式版本（format != 1）或结构非法的包一律拒绝**，不落库；导入落到本 chat 自己的命名空间（同名不拒绝，不触碰其他 chat 的模板）。
- 模板命名空间（MYC-67 / MYC-83 / MYC-91）：模板键为 `<属主 chatID>/<name>`，各 chat 的同名模板互不可见。save/import/apply/export 只在本 chat 命名空间内解析；同名不会被拒绝，因此无法借此探测他群是否使用某名称（MYC-80 R5 存在性 oracle 已消除）。属主 chatID 可以为负数（超级群/频道的 chat ID），仅属主 0 视为无属主：save 拒绝，加载时 fail-closed 拒绝启动。加载时，旧版裸名称且带属主的记录自动迁移到命名空间键（单事务；若命名空间键已存在则拒绝启动，不做猜测）。无属主记录仍 fail-closed 拒绝启动。
- 无属主模板的诊断与隔离（MYC-83）：服务停止后执行：
  - `teleadmin -config <file> -db-diagnose`：只读报告，按命名空间、旧版待迁移、无属主、损坏、已隔离分类，不修改数据库。
  - `teleadmin -config <file> -db-quarantine-ownerless-templates`：把无属主记录原样移入 `templates_quarantine` 表后退出；不删除任何数据，之后服务可正常启动。隔离表只供人工查看或恢复，存储层从不读取它；若目标键已存在于隔离表则拒绝覆盖。
  - 两个命令都会获取单写者锁；服务运行时执行会失败（锁拒绝由 `internal/store/template_maintenance_test.go` 覆盖）。隔离记录逐字节保留原始 JSON（同文件保留校验），无属主判定只看属主 0，负数属主不被隔离。
- 语言/时区获取新值（MYC-66 修复）：应用档案后 `/lang`、`/tz` 仍独立修改 chat 的当前生效语言/时区；`/template save`、`/clone` 构造内容时以当前 Store 生效的语言与时区覆盖档案中的陈旧值，未应用档案的 chat 使用其默认值。
- 版本化记录：chat 配置档案（profiles 表）与模板（templates 表，含属主 chatID）每次写入都由 Store 递增版本号；重启后版本号与属主一并延续（基于 `db.path`）。
- 原子性：应用配置时，profiles 记录与 chats 设置记录在**同一 FileStore 事务**中提交；提交失败两者一并回滚，内存状态仅在提交成功后更新（提交失败注入测试覆盖：内存与磁盘均保持原值）。模板结构校验在任何写入之前完成。
- 每生效期：模板/克隆应用后，该 chat 的语言/时区/欢迎语/关键词黑名单即按档案生效——关键词黑名单（非 nil 时）与全局配置替换为本 chat 的档案值；欢迎语与启用开关同样按档案覆盖；未应用档案的 chat 维持全局行为。
- 模板名称限制：1–32 个字母/数字/`_`/`-`；档案载荷上限 3000 字节（保证导出包可放进一条 Telegram 消息），关键词 ≤ 100 条、每条 ≤ 64 字符，欢迎语 ≤ 1024 字符。
- 名片、发布任务等其他数据不属于“Bot 配置/规则”，不在克隆范围内（克隆的是配置与规则档案本身）。
- 自动测试：`internal/store/profile_test.go`（结构校验、原子性拒绝非法模板、版本递增、模板存取、模板属主隔离（跨 chat 读取 not-found、同名另建档、属主可覆盖、属主 0 拒绝）、属主跨重启延续、无属主旧记录 fail-closed 加载、未知版本拒绝、持久化往返、提交失败回滚注入）、`internal/store/template_group_chat_test.go` 与 `internal/bot/template_group_chat_test.go`（负数群组/频道 chat ID 的 save/import/export/apply、重启后加载、diagnose 归类为命名空间且 quarantine 不误移）与 `internal/bot/clone_test.go` 及 `internal/bot/template_ownership_test.go`（匿名/普通成员/单侧管理员拒绝（含源侧校验）、克隆后规则与欢迎语实际生效、未知版本与坏 JSON 拒绝、导出导入往返+应用生效、未知模板与用法提示、跨群越权（export/apply 返回未找到且不泄露、save/import 落在闯入 chat 自己的命名空间且不触碰原属主记录）、export 审计日志、应用档案后 /lang 与 /tz 使 save/clone/export 取新值）。

## 持久化与崩溃恢复的覆盖范围（MYC-83）

- 每次提交先写入随机命名的临时文件并 fsync，再原子 rename 覆盖数据库文件，并尽力 fsync 所在目录（best-effort：目录打开或同步失败不会否定已成功的 rename 提交）。失败时数据库文件保持不变。
- `store/file_store_kill9_test.go` 启动子进程持续提交，收到足够多的提交确认后发送 SIGKILL，再重新打开数据库。它验证：已确认的提交全部存在；记录是连续前缀；内核在进程退出后释放单写者锁，新进程可以打开。
- **不覆盖**：断电、内核崩溃或存储设备缓存丢失。这些只能通过运维验证，测试无法在用户态观察。测试也不判定被中断的那一次提交最终是否落盘（两种结果都被接受，只要数据库一致）。

## 后续功能路线图（MYC-46）
各能力独立验收。首期 MVP 验收不代表下列能力已实现。依赖顺序：`P0 持久化基础` 与 `S1 成员管理` 为前置；`S2`、`S3` 依赖 `P0`；`S4` 依赖 `P0` 与 `S1` 的管理员校验。

| 编号 | 能力 | 依赖 | 持久化需求 | 验收条目 |
|---|---|---|---|---|
| P0 | 持久化基础（使 `db.path` 生效） | 无 | 新增存储层（如 SQLite），保存 chat、target、卡片、规则、模板与发布记录；重启后数据保留 | 重启后数据可读；并发写入有测试；未配置 `db.path` 时保持内存模式并明确标注 |
| S1 | 成员管理与权限控制 | 无 | 无（状态在 Telegram） | 见上节。后续可补 `/ban`、`/unban` 与带时长的限制 |
| S2 | 文本/媒体多目标发布 | P0 | 目标 chat 注册表、发布任务与逐目标结果 | 仅群管理员或属主可发布；支持文本、图片、视频、文档；按目标返回逐条结果，部分失败不报告整体成功；目标数量与内容长度设上限；测试覆盖每类媒体、部分失败、非管理员拒绝（**已实现**，见上节，待独立检查） |
| S3 | AI 名片创建/展示/查询/编辑 | P0 | 名片表，按（chat、用户）唯一，保存字段与更新时间 | 用户只能编辑自己的名片（所有权校验）；字段校验与长度上限；按关键字查询；未找到与越权编辑返回明确错误；不得在未获批准时调用外部模型服务；测试覆盖创建、展示、查询、编辑及越权拒绝（**已实现**，见上节，待独立检查） |
| S4 | Bot 配置、规则和模板克隆 | P0、S1 | 配置、规则与模板的版本化记录 | 仅源与目标 chat 的管理员可克隆；克隆前校验模板结构，失败时不做部分写入（原子性）；导出/导入使用版本号；测试覆盖权限拒绝、非法模板、部分失败回滚（**已实现**，见上节，待独立检查） |

推荐执行顺序：S1（已完成实现，待独立检查）→ P0 → S2 与 S3（可并行）→ S4。每个切片完成后需测试工程师验证、代码审查工程师复审、安全工程师检查后方可关闭。
