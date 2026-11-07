# 龙道 AI 迁移至华为云：方案与执行结果

## 2026-11-03 执行结果（以本节为准）

**已完成部署及域名切换。** 以下原调研章节保留为历史记录，其中“尚未执行”“当前只读”等表述不代表最终状态。完整交付及运维方法见 [迁移交付记录](relay-migration-20261103.md)，执行前原文另存于 [调研归档](migration-20261103/pre-execution-research.md)。

后续自动部署迁移已完成：**GitHub production Environment 现已指向新机，配套独立运行时发布器已安装，并于北京时间 17:50 通过实际 GitHub 部署 job 验收。** 当前 blue（18081）接管，green 已停止。详见 [自动部署交付](relay-autodeploy-migration-20261103.md)；[核查记录](relay-deployment-config-audit-20261103.md) 保留修复前问题。

| 项目 | 执行结果 |
|---|---|
| 目标 | 华为云 `121.37.128.126`，`https://ai.longdaoec.com` 已上线 |
| 旧入口 | `https://longdaoai.cn` 经旧机 Caddy 转发新站；原 Key 验证通过 |
| 维护窗口 | 北京时间 2026-11-03 16:37:19—17:03:04，约 25 分 45 秒 |
| 数据 | 80 张 public 表的全部行数及内容指纹、65 个序列状态，启动新应用前与冻结旧库完全一致 |
| 规模 | 用户 22、API Key 74、上游账户 47、设置 271、支付订单 28、使用日志 220,914 |
| 版本 | 保持 `ghcr.io/yaoruiquan/sub2api:2026.10.21-2`，未升级 |
| 存储 | 应用、PG、Redis、镜像、容器层、日志及备份均已核实位于 `/www` 数据盘 |
| SEP | 六个现有容器持续运行，重启次数均为 0；入口 Caddy 仅热加载 |
| 上游 | 按要求原样保留，未继续排查或进行付费模型调用 |

最终采用本项目独立 `longdao-containerd.service` / `longdao-docker.service`，持久目录位于 `/www/longdao/runtime`，专用 Docker socket 为 `/run/longdao-docker.sock`。两个服务已启用开机启动，要求 `/www` 为实际挂载点。未移动 SEP 的原 Docker/containerd。

独立 Docker 禁用默认 bridge 和 iptables 管理，容器使用 host 网络：PG 仅监听 `127.0.0.1:15432`，Redis 仅监听 `127.0.0.1:16379`；应用监听 SEP 网桥网关 `172.19.0.1:18080`，由 `172.19.0.2` 的现有 Caddy 代理，已加入对应 UFW 规则。未新增数据库、Redis 或应用的公网监听。独立 Docker 重启恢复演练在正式切换前通过；未做生产整机重启。

最终冻结后，旧应用正常退出，数据库无残留业务连接、无待处理支付订单。最终备份用严格单事务模式恢复至空库；先完成全量指纹核对，再仅修改 `api_base_url`、`frontend_url`、`balance_low_notify_recharge_url` 到新域名。JWT、TOTP、余额、密钥、OAuth、支付回调及上游配置均保留。Redis 从最终 RDB 恢复，原过期时间保留，新机设置独立密码并开启 AOF，重写及写入状态正常。

最终数据库清单 SHA256：`c58a8bc8ccb2ea0daab53d917e0a78fe5ec02e35f7900b6cf50d540330e8002f`。新旧域名首页、健康检查返回 200；原 API Key 在两边 `/v1/models` 均返回 200、7 个模型，响应指纹与迁移前一致。新站未认证模型目录返回 401。SEP 两个域名首页均返回 200，新项目三个容器均 healthy、重启 0、无 OOM。

原始最终备份保留在旧机 `/opt/longdao/migrations/20261103/final`、新机 `/www/longdao/backups/20261103-final`，并复制到本地私密目录 `/Users/yao/.codex/backups/longdao/20261103-migration`。部署配置含私密信息，仅存服务器和私密备份，不写入仓库文档。

旧机 green/blue 应用停止且 restart policy 为 `no`；旧 Caddy、共享 PG、Redis 及其他服务继续运行。旧域名仍依赖旧机，不能直接释放旧服务器。**新站已承接写入，不能直接启动冻结旧库回滚；必须先冻结新站并反向迁移最新数据。** 旧机 Redis 供其他服务共用，禁止整份覆盖。

未执行付费模型、真实支付、邮件、OAuth 外部登录及后台人工登录验收。迁移快照已保留；本次未新增长期定时备份任务。

---

## 历史：执行前调研与计划

日期：2026-11-03。首次现场只读核查：北京时间 15:10—15:19；后续复测：15:31 起。

以下为执行前的历史调研：当时尚未实施迁移，仅进行只读检查与探测。最终状态以上方执行结果及交付记录为准。

## 1. 结论、已确认事项与实施边界

可以迁移。目标机是 8 核、约 32 GB 内存的 x86_64 Ubuntu 服务器，已有 Docker；当前应用镜像同为 linux/amd64。用户新增硬性要求：**本项目持久内容及容器镜像/数据必须放在 1 TB 挂载数据盘，不放系统盘**。16:00 已确认数据盘挂载于 `/www`，可用约 920 GB；此前 162 GB 是系统盘可用空间，不作为项目落盘位置。首次迁移采用**线上原版本、完整数据库、应用数据及 Redis 状态迁移，短暂停服后切换到唯一账本**。

目前有三个不能略过的现场事实：

1. 新机已经承载 SEP，`sep-caddy` 占用 80/443。龙道 AI 要新增独立应用、数据库和 Redis，复用现有 Caddy 入口；不能直接整栈启动仓库里的生产 Compose。
2. `api.a6api.com` 从新机连接异常，而从旧机访问正常，涉及 9 个 active 上游账户。**按用户再次确认，暂不处理该问题，本轮不继续探测、不修改上游、不再要求用户为此作选择。** 历史结果保留供后续处理；迁移技术条件单独评估，不把上游未验证写成已通过。
3. Redis 存放已有任务绑定、刷新令牌及部分额度状态，不能当成可丢缓存。现有 `backup.sh` 只备份 PostgreSQL 与应用文件，不足以完成本次迁移。

| 已确认事项 | 用户决定 / 现场情况 | 执行约束 |
|---|---|---|
| 正式域名 | 用户已明确确认 `ai.longdaoec.com`，并表示已在华为云备案 | 按此域名准备；不使用 `.co` |
| 可用停服窗口 | 用户已同意中转短暂停服，要求暂不讨论具体时段 | 先提供工作量估算，再通过恢复演练测时；不重复索要时间窗口 |
| 旧域名如何保留 | 用户已确认保留 `longdaoai.cn`；新机 SEP 仍调用该域名 `/v1` | 旧入口转发新站，保留原客户端访问方式 |
| 迁移内容 | 全部用户、余额、原 API Key、上游及业务配置 | 保留整个数据库、Redis、文件与有效密钥，不重新发 Key 或重置余额 |
| 存储位置 | 全部项目持久内容放 1 TB 数据盘 `/www` | 包括镜像、容器层及日志；运行时存储迁移尚未执行 |
| 上游问题 | 用户要求先不管 | 暂停该问题的排查；现有探测结果保留，迁移准备继续按独立条件评估 |

### 最新澄清：SEP 停机并非项目迁移的必要条件

此前 SEP 停机来自我提出的“移动整台新机共享 Docker/containerd 存储”方案，不是迁移龙道 AI 本身的必然要求。**将整机运行时迁移移出本次默认范围**；优先为本项目设计独立运行时与数据盘存储，保持 SEP 原运行时、数据库和 Redis 不变。这个独立方案尚未实施或验证，不提前承诺网络、入口及重启恢复已通过。

代理接入必须相应重新设计：独立 Docker 运行时不能直接使用另一个 daemon 的 `sep-migration-net`。需选定并验证 Caddy 可达、未暴露公网、访问受限的跨运行时入口，再补齐最终 Compose 和网络配置。如果隔离方案验证不通过，先报告具体阻塞，不自行停止 SEP 或退回整机迁移方案。

现阶段没有需要用户立即补充的信息。域名、数据范围、数据盘位置和旧域名策略都已明确；时间由我先估算，上游按要求暂缓。当前范围仍是调研与文档，不据本节自动开始生产变更。

### 工作量与中断时间估算（尚未经演练验证）

| 阶段 | 初步估算 | 服务影响 |
|---|---|---|
| 数据盘独立运行环境、镜像传输、备份及恢复演练、代理配置准备 | 约 2—4 小时 | 旧中转继续服务；不以停止 SEP 为前提，备份/传输需控制负载 |
| 最终冻结、排空、最终同步、数据核对与入口切换 | 约 10—30 分钟 | 中转短暂维护，SEP 依赖中转的模型调用同步受影响，SEP 自身不因此整站停机 |
| 恢复后的观察与复核 | 约 30—60 分钟 | 正常服务下观察，无额外计划停机 |

上述是工作计划估算，不是实测或完成保证；独立运行时设计、网络传输、未排空的长任务、TLS/回调适配都可能增加耗时。准备时间不等于停机时间。执行恢复演练后，用实测替换估算，不再要求用户现在给出具体维护时段。

无须现在增加的确认：付费测试默认不执行，不发送真实邮件/支付测试、不调整余额或价格；需要真实付费验收时再明确测试账户与预算。旧机、兼容入口和回滚备份默认保留，暂不删除；保留期在稳定性验收后再确定。OAuth/支付服务商的外部控制台配置若无法从现有配置核实，准备具体变更清单后再请用户协助，不要求用户现在提供所有账号或密钥。

本方案按用户已同意的短暂停服执行，不承诺跨服务器零停机。本轮仍只更新调研文档，未开始迁移。

### 15:57 项目迁移条件复核（按用户要求，暂不计上游连通性）

**基础条件已满足，可以开始准备、传输与恢复演练；正式切换前的部署和验收尚未完成。** 上游问题单独记录，本节不把它作为项目迁移准备的阻塞。

| 条件 | 状态 / 复核证据 |
|---|---|
| 两台机器 SSH 权限 | 满足，已再次只读登录 |
| 新机资源与架构 | 满足，8 核 / 约 32 GB，linux/amd64 与原镜像兼容；16:00 补充确认实际项目目标数据盘 `/www` 可用约 920 GB |
| Docker / Compose | 满足，已安装可用 |
| 正式域名及解析 | 用户确认 `ai.longdaoec.com`，公共 A 记录仍指向 `121.37.128.126` |
| 域名备案 | 用户已确认在华为云备案；本轮未读取备案控制台独立验证 |
| 停服与旧域名策略 | 用户同意短暂停服、保留旧域名 |
| 原站数据、配置、备份工具 | 原站 green healthy，PG 18.4 可读，80 张 public 表约 1355 MB；env/Compose/Caddy/活跃色文件存在，pg_dump/pg_restore 18.4 可用 |
| 数据库/Redis 镜像 | 新机已有与旧机 ID 完全一致的 PG 及 Redis 镜像，可新增独立实例，不需依赖在线拉取 |
| 应用镜像 | 旧机镜像完整；新机尚未导入，待传输并校验 |
| 目标独立环境 | `/www/longdao` 仍不存在；专用卷、网络、PG/Redis 和应用 Compose 尚未创建 |
| 新站点 HTTPS | 尚未完成：Caddy 运行配置仅有 SEP 两个域名，AI 域名握手仍失败；需新增站点及证书 |
| 完整备份与恢复 | 本轮尚未制作/传输最终或演练备份，尚未进行实际恢复、全量对账和回滚演练 |
| 兼容入口与业务配置 | 旧域名转发、settings/OAuth/支付相关调整与 SEP 调用验收尚未执行 |

15:57 新机 SEP 容器仍运行，Caddy 文件 SHA256 与首次核查相同；这支持按当前入口设计准备，但实施时仍须再次复核。以上未完成项属于接下来的迁移工作，不是要求购买新机器、重新选域名或更换数据库版本的前置缺口。迁移与切换仍分阶段进行，不能将“基础条件满足”记作“恢复与上线验收已通过”。

### 16:00 新增存储要求与现网影响

用户要求项目全部放到 1 TB 数据盘。只读核查结果：

| 内容 | 实测 / 要求 |
|---|---|
| 系统盘 | `/dev/sda1` → `/`，197 GB；不作为本项目数据目录 |
| 数据盘 | `/dev/sdb1` → `/www`，原始容量约 1.07 TB，文件系统显示 984 GiB，可用约 920 GiB |
| 挂载持久化 | `/etc/fstab` 已按 UUID 配置 `/www` 的 ext4 挂载 |
| 项目根目录 | `/www/longdao`，归档、部署配置、私密配置、备份、应用数据、媒体与日志均放其下 |
| 当前 Docker Root Dir | `/var/lib/docker`，仍位于系统盘 |
| 当前镜像/容器快照 | 运行挂载显示实际使用 `/var/lib/containerd/...`，也仍位于系统盘 |
| 结论 | 业务数据可直接安排到数据盘；**全部持久内容含容器层落数据盘的要求尚未完成，不能因 Compose 位于 `/www` 就宣布满足** |

目录规划：`/www/longdao/deploy`、`app-data`、`postgres`、`redis`、`backups`、`logs`、`images`、`releases`、`tmp`。采用明确的 bind mount，不使用默认落入 `/var/lib/docker/volumes` 的持久卷。PostgreSQL 18 建议把其整个 `/var/lib/postgresql` 挂至本项目数据盘目录，同时保持有效 PGDATA 对应子目录，避免镜像声明的父目录匿名卷回落系统盘；实际恢复时以校验后的布局为准。

“全部”还包含镜像、容器可写层及容器日志。最新默认方向是本项目专用运行时，其 Docker/containerd 持久目录均位于 `/www/longdao/runtime/`，业务数据仍采用明确 bind mount。专用 socket、状态目录、网络与服务启动参数需要在实施方案中隔离并验证；不得因设置了 Docker `data-root` 就假定 containerd 内容也已迁移 [O6]。

此前 `/www/runtime/docker`、`/www/runtime/containerd` 的整机共享运行时迁移方案**不再作为本次默认方案**。它会涉及现有 SEP 容器停机，未获本次实施授权。当前没有移动任何 Docker/containerd 目录，没有更改其配置，也没有停止 SEP。独立运行时方案先完成设计与验证，再定最终配置；共享网络接入示意不能原样沿用。

启动与备份前必须验证 `/www` 真实挂载到预期数据盘；配置挂载依赖及失败即停止，避免数据盘挂载失败时在系统盘的空 `/www` 目录继续写入。验收用 `findmnt -T`、实际 Mounts、Docker Root Dir、containerd 配置和运行挂载逐项核实，不只看目录名称。

对服务的影响分开判断：

- **旧机当前中转**：16:00 复核仍 running/healthy。本轮只有只读检查和文档修改，无配置或流量变更。准备/传输/隔离恢复演练期间不主动停旧服务；在线备份仍有 CPU/磁盘 I/O 负载，应低峰并限速，不承诺性能绝对无影响。
- **最终中转切换窗口**：需短暂停止新请求、排空在途请求及后台账务、同步最终数据再放量；窗口内会有请求失败或需重试，依赖中转的 SEP 模型调用也会受影响。时长等演练测得，不能承诺全程不中断。
- **新机 SEP**：默认保持其原运行时、数据与容器运行；采用本项目独立运行时方向，先验证存储和跨运行时代理接入。中转最终维护期间，其模型功能依赖会受影响；这不同于停止 SEP 整个站点。不自行实施整机 Docker/containerd 存储迁移。
- **迁移后**：用户/余额/Key/配置以新库为唯一权威，旧域名继续转发；旧应用保持停止，避免双写。此前确认的上游连通性问题仍单独存在，按用户要求本轮暂不处理，不能据“项目迁完”承诺模型业务完全等同旧机。

## 2. 已核实的现场清单

以下事实来自本轮 SSH、Docker inspect、只读 SQL 和网络探测，作为现场证据 R1—R4。数量会随业务增长，不能作为切换时的最终账务基线。

### R1：旧服务器

| 项目 | 实测值 |
|---|---|
| 地址 / SSH | `64.83.39.223`，本机别名 `longdao`，root / 22 |
| 系统 / 资源 | Debian 12，x86_64，4 核，约 8 GB 内存；根盘 140 GB、可用约 40 GB |
| Docker / Compose | 29.6.1 / v5.3.1 |
| 部署目录 / 项目名 | `/opt/longdao/deploy/production` / `production` |
| 活跃实例 | `longdao-sub2api-green`，healthy；`.active_color=green` |
| 当前镜像 | `ghcr.io/yaoruiquan/sub2api:2026.10.21-2` |
| 镜像 ID / RepoDigest | `sha256:795dd0d4f6b740db70fc9c0f46d49c4a157aa172608f75436d6c97757172635e` |
| 源码 revision | `5fd22ed0e7ee744ea37516609e9730ec370595e0`，与本轮本地 HEAD 一致 |
| 应用镜像体积 / 架构 | 75,482,311 字节 / linux/amd64 |
| 站点 | `longdaoai.cn`，由 `longdao-caddy` 提供入口 |
| 应用文件 | `longdao_sub2api_data` → `/app/data`，约 44.9 MB |
| PostgreSQL | `longdao-postgres`，实际运行 PostgreSQL 18.4，自定义 pgvector 镜像 |
| PostgreSQL 镜像 | `longdao-postgres-pgvector:18.4-v0.8.1-9a8afca54e78` |
| 数据目录 | `SHOW data_directory` 为 `/var/lib/postgresql/data` |
| 本项目数据库 | `sub2api`，约 1353 MB，80 张 public 表；扩展 `plpgsql`、`pg_trgm` |
| 其他数据库 | 同一 PostgreSQL 还有 `sep_prod`、`sep_dev` 等，不能覆盖、迁走或停止它们 |
| Redis | `longdao-redis`，Redis 8，应用使用 DB 0；旧 SEP 还使用 DB 2，DB 1 也有队列数据 |
| Redis 实际持久化 / 认证 | `CONFIG GET` 显示 AOF 未启用，requirepass 未配置；与仓库/容器启动参数表达的预期不同 |
| 其他运行服务 | 旧 SEP backend/web 与共享 PostgreSQL、Redis、Caddy |

Redis DB 0 现场观察到 `sched:`、`sticky_session:`、刷新令牌、`billing:`、`apikey:`、`concurrency:` 等前缀，也观察到 `bull:` 键；因此连 DB 0 都不能未经归属核实就批量清理。现场约数：DB 0 有 2800 余键，DB 1 有 17 键，DB 2 有 4 键。

`/app/data` 包含 `config.yaml`、`.installed`、模型价格文件、日志、`sora/`、`pages/` 等目录。当前生成的 config.yaml 顶层没有 S3 配置，但这不足以排除所有数据库配置、外部对象存储与历史外链；迁移前仍须核对历史媒体实际存储位置。

### R2：新服务器

| 项目 | 实测值 |
|---|---|
| 地址 / SSH | `121.37.128.126`，root / 22 |
| 可用本机密钥 | `/Users/yao/.ssh/sep_deploy`；已只读登录成功，无需再次提供密码或私钥 |
| 系统 / 资源 | Ubuntu 24.04.4，x86_64，8 核，约 32 GB 内存；根盘 197 GB、可用约 162 GB |
| Docker / Compose | 29.9.2 / v5.6.1 |
| 已有项目 | Compose 项目 `sep-migration`，部署目录 `/www/sep/deploy` |
| 运行容器 | `sep-green-web`、`sep-green-backend`、`sep-downloads`、`sep-postgres`、`sep-redis`、`sep-caddy` |
| 网络 | `sep-migration-net` |
| 公网入口 | `sep-caddy` 已绑定 80/443；宿主 UFW 已允许这些端口 |
| Caddy 主配置 | `/www/sep/deploy/Caddyfile`，容器内 `/etc/caddy/Caddyfile` |
| Caddy 运行中域名 | `sep.longdaoec.com`、`longdaosep.cn`；运行配置中尚无 AI 域名 |
| 目标数据库现场 | `sep-postgres` 18.4，目前只有 `postgres` 和 `sep_prod`，没有 `sub2api` |
| 拟用新目录 | `/www/longdao` 当前不存在，可以作为独立部署目录 |
| SEP 对中转的依赖 | backend 的 `SUB2API_BASE_URL` 主机仍为 `longdaoai.cn`，路径 `/v1`；已有 API Key 配置 |

目标 Caddy 文件本轮 SHA256：`4f4868f7a4cd4c3cfa8e68bc3e742f3c472e26c31c6d8e1b4b6431fdddc4586a`。这是调研快照，不是对未来文件状态的保证；执行前要重新读取，合并届时的配置，避免覆盖并行 SEP 部署产生的改动。

### R3：DNS、TLS 与上游实测

公共 DNS 查询：`ai.longdaoec.com` 的 A 记录为 `121.37.128.126`，TTL 300 秒；`ai.longdaoec.co` 返回 NXDOMAIN。最初本机普通 DNS 得到过 `198.18.0.37`，因此判断公网解析采用公共 DNS 查询和 `curl --resolve` 固定目标 IP，避免本地代理 DNS 影响。

新机上的 `.com` HTTP 请求返回 Caddy 308 跳转 HTTPS；HTTPS 握手失败。结合 Caddy 运行配置尚无该站点，符合“新域名站点/证书尚未就绪”的现状；证书日志、CA 可达性及最终原因仍须实施阶段确认。不能把这个 HTTP 响应当成 AI 服务已部署。

未携带凭证访问 `https://<上游>/v1/models`：

| 上游 | 新机结果 | 旧机对照 / 解释 |
|---|---|---|
| `api.a6api.com` | 两次 TCP 443 连接超时，HTTP 000 | 旧机同路径返回 401；存在实际迁移阻塞 |
| `api.a6api.vip` | 401 | 旧机也返回 401 |
| `aipro.hk.cn` | 401 | 证明该探测路径可到达 HTTP 服务 |
| `api.naoshengyun.com` | 401 | 同上 |
| `mixapi.io` | 401 | 同上 |

401 只证明 DNS/TCP/TLS/该路径的 HTTP 连通性，不证明凭证有效、模型权限、推理成功、SSE 稳定、媒体下载或所有平台路径可用。HEAD 探测部分根路径还曾返回 403/404，因此评估以具体 API 路径为准。

`api.a6api.com` 的新旧机 DNS 返回集合不同；仅凭本轮证据不能确定是 DNS、线路还是平台策略问题。新机最终探测返回过 `192.133.77.145` 及 IPv6 地址，旧机成功连接的地址为 `172.80.127.126`。不要据此固定 hosts、关闭 TLS 验证或擅自换入口。

未删除账户共 21 个，均为 `apikey` 类型，其中 20 个 active、1 个 error；active 账户均未绑定代理。20 个 active 账户分布为：`.com` 9 个、`.vip` 4 个、`aipro.hk.cn` 3 个、`api.naoshengyun.com` 2 个、`mixapi.io` 2 个。其中 `.com` 为 OpenAI 6 + Anthropic 1 + Grok 1 + Gemini 1；切换前重新查询确认，不以本轮快照代替最终清单。

#### 15:31 起的补充复测

用户询问是否已实测后，再次直接 SSH 到新旧服务器复测，不经本机网络代理：

- 新机 `api.a6api.com/v1/models` 再次连接超时，HTTP 000；旧机同路径返回 401。
- 新机另外四个域名同路径仍全部返回 401；仅说明未认证路径可达，没有携带 API Key 验证推理。
- 再次只读查询数据库确认受影响为 OpenAI 6、Anthropic 1、Grok 1、Gemini 1，共 9 个 active 账户。这是账户/渠道数量，不是“9 个模型”，也不证明某个平台所有模型均不可用。
- 为区分普通解析失败与线路/TLS 问题，单次请求通过 curl `--resolve` 指向旧机刚刚成功连接的 `172.80.127.126`，保持原域名和默认 TLS 校验，不修改 DNS/hosts。新机该请求仍被连接重置（curl 35），未得到 HTTP 响应。因此目前不能认定“仅改 DNS 即可解决”。curl 选项行为来源：`https://curl.se/docs/manpage.html`。
- 新机向 Google/Cloudflare 的 DNS-over-HTTPS 查询也未成功，不能用它们的失败推断上游的权威 DNS 结果；本轮仍不确定具体根因。

结论：按当前配置直接切换，会让这些渠道面临请求失败；应先解决连接问题或验证平台认可的替代入口。其他渠道的同类模型是否可以接替，要进一步核对权限、模型映射、分组和价格，不能直接宣称自动无损接替。

#### 15:37 的同 IP、不同域名对照

进一步定位到域名相关的 TLS 连接差异；所有测试都没有携带 API Key，没有修改 DNS/hosts 或生产配置：

| 从新机连接同一地址 `172.80.127.126:443` | 结果 |
|---|---|
| 纯 TCP 连接 | 成功，约 0.18 秒 |
| TLS server name = `api.a6api.com` | 连接重置；OpenSSL errno 104，curl 35，未收到 HTTP 响应 |
| TLS server name = `api.a6api.vip` | TLS 1.3 成功、证书校验通过；对应 API 路径返回 401 |

旧机对同 IP 的 `api.a6api.com` TLS 握手成功，证书校验通过。新机普通 DNS 此次还返回了 `103.214.168.106`，再次与旧机解析结果不同。

这组证据说明至少存在“随域名变化的 TLS 连接结果”，不支持把问题归因于模型本身或 API Key 失效；使用固定 IP 也没有让 `.com` 恢复。**推断**：更像该域名相关的 DNS/链路过滤或平台接入策略；目前无法从连接重置判断具体由哪一方造成，不能断言是华为云主动拦截，也不能断言单纯改 DNS 就能修复。TLS server_name 机制参考 RFC 6066：`https://www.rfc-editor.org/rfc/rfc6066.html`。

### R4：业务与登录依赖

| 数据 | 本轮精确 count |
|---|---:|
| users | 22 |
| api_keys | 74 |
| accounts（含已删除） | 47 |
| groups | 39 |
| settings | 271 |
| payment_orders | 28 |
| redeem_codes | 52 |
| usage_logs | 219701 |
| usage_billing_dedup | 244905 |

这些精确计数是非冻结状态的逐条查询结果；现场 `pg_stat_user_tables` 只是估计值，部分表与精确 count 不同。最终验收必须重新取得冻结后的同一基线。

业务设置中 `api_base_url`、`frontend_url`、余额充值链接、Google/GitHub OAuth callback 仍引用旧域名。Google/GitHub OAuth、支付、邮件验证当前开启；本轮订单状态没有 pending，但迁移窗口仍要重查。

SMTP 为 `smtp.qiye.aliyun.com:465`，新机 TLS 握手成功、证书校验通过；没有登录邮箱或发送邮件。Google/GitHub 登录的浏览器可达性、邮件实际投递及支付回调均未验证。

## 3. 迁移范围与数据边界

### 必须完整保留

| 内容 | 处理方式 | 原因 |
|---|---|---|
| `sub2api` 整库 | PostgreSQL 18.4 custom-format 逻辑备份，恢复到新机空库 | 保留全部用户、密码哈希、余额/冻结余额、累计充值、原 API Key、分组、定价、权限、订阅、订单、兑换码、账户凭证、历史用量、去重账务、审计与 schema 记录 |
| 原数据库身份/对象 | 保存本应用角色、owner、必要 ACL、扩展清单并复核 | 单库 dump 不包含集群全局角色；不把旧集群角色整体导入新共享设施 [O1] |
| Redis 状态 | 一致边界快照，恢复到**独立的龙道 AI Redis** | 保留视频/图片任务绑定、登录刷新状态、额度与 TTL |
| `/app/data` | 完整归档，保留属主、权限及目录层级 | 运行时配置、媒体、本地页面与价格文件都可能依赖它 |
| 私密配置 | `.env`、实际 config.yaml、有效运行时配置私密保存 | 原 JWT 与 TOTP/支付加密密钥必须保留，不能换成模板随机值 |
| 部署与镜像 | 原 Compose/Caddy/活跃色、线上镜像及 SHA256 | 原样迁移可避免混入新功能或镜像仓库不可达问题 |
| 外部引用 | 对象存储、外链、支付/OAuth、邮件、客户端及 SEP | 数据库迁完不代表这些外部依赖已切换 |

代码依据：用户及 API Key schema 在 [user.go](../backend/ent/schema/user.go)、[api_key.go](../backend/ent/schema/api_key.go)；账户凭证位于 [account.go](../backend/ent/schema/account.go)。迁移只导出这几张表会遗漏关联数据，所以使用全库备份。

JWT 的数据库持久值会优先于不一致的环境变量，见 [security_secret_bootstrap.go](../backend/internal/repository/security_secret_bootstrap.go)。TOTP 加密密钥还用于支付配置加密和恢复链接签名，见 [payment/wire.go](../backend/internal/payment/wire.go)。文档与仓库中不得写入真实密钥、用户邮箱、原 API Key 或上游凭证。

### Redis 的明确处理方案

默认采用**旧实例完整 RDB 快照 → 新建隔离 Redis 恢复**，保留过期时间，不覆盖新机 `sep-redis`，不启动 DB 1/2 的任何 SEP 消费者。快照可能包含旧 SEP/队列键，要作为私密数据处理，并在记录中明确这些是归档附带状态，不能被误认为 SEP 迁移成果；若要求不带这些状态，实施前需改为经过演练的本项目键级迁移方案。

旧 Redis 无需停止。可以在冻结本项目写入后通过 `redis-cli --rdb` 生成快照，检查退出码与文件完整性 [O3]；这是实施步骤，本轮没有执行。旧 SEP 仍可能写 DB 1/2，其变化不属于本项目一致性保证。DB 0 内的 `bull:` 键归属也须复核，不能随意删除。

恢复时先停止目标 Redis，在新建且无 AOF 的干净数据目录放入 RDB，核对权限，然后以 `appendonly=no` 启动加载；验证 DB 0 关键状态、任务绑定和 TTL 后，再在线启用 AOF，等待重写成功并确认启动配置也持久设置为启用 AOF。不要直接以 `appendonly=yes` 首次启动来恢复单独的 RDB，也不能在运行中的 Redis 数据目录放入文件后就假定已加载。已有 AOF 与 RDB 并存时的加载优先级按官方说明处理 [O4]。新 Redis 要配置真实有效的认证，只加入本项目私有网络，不发布公网端口；复核应用实际认证配置。

代码依据：[openai_videos.go](../backend/internal/service/openai_videos.go) 将任务与原账户关联，TTL 为 7 天；[image_task_store.go](../backend/internal/repository/image_task_store.go) 存图片任务；[refresh_token_cache.go](../backend/internal/repository/refresh_token_cache.go) 存刷新/撤销状态；[user_platform_quota_flusher.go](../backend/internal/service/user_platform_quota_flusher.go) 将部分 Redis 额度快照刷入数据库。

## 4. 新机部署布局

新增 `/www/longdao/`，位于已核实的 1 TB 数据盘，独立 Compose 项目名 `longdao-ai`。应用沿用原 service 名称及 blue/green 机制，首次启用 green；使用数据盘 bind mount。本项目独立运行时及跨运行时入口尚待技术验证，不迁移 SEP 所用共享运行时。

**下图仅表达入口关系，跨运行时连接地址尚未选定；不能据图直接部署。**

```text
现有 sep-caddy（继续独占公网 80/443）
  ├─ sep.longdaoec.com / longdaosep.cn → 原 SEP 路由
  └─ ai.longdaoec.com → 待验证的受限跨运行时入口 → AI green:8080
                              │
                       新建龙道 AI 私有网络
                          ├─ 专用 PostgreSQL 18.4
                          └─ 专用 Redis 8
```

- 独立运行时不直接加入 SEP daemon 的 `sep-migration-net`。明确并验证跨运行时受限代理地址及访问规则；数据库与 Redis 仅接本项目私有网络。
- 应用、PG 与 Redis 都不发布宿主公网端口，不新增第二个 80/443 监听。
- 保留 PG 18.4、`pg_trgm` 和 `PGDATA=/var/lib/postgresql/data`。不要照仓库里的浮动 `postgres:18-alpine` 拉取后顺便升级。
- 可以导入旧机现有应用/PG/Redis 镜像并校验 ID，避免依赖新机直接下载 GHCR、Docker Hub；镜像导入是实施阶段动作。
- 为 AI 服务设置资源限制和日志轮转；保留 SEP 的现有配置。具体上限在生成 Compose 时按恢复演练与负载制定。
- Caddy 只新增 AI 站点，保留已有 SEP/downloads/WebSocket 路由、TLS 和日志配置，不整体覆盖文件。

## 5. 实施步骤与停止条件

### 阶段 A：上线前准备与恢复演练

1. 使用已确认的 `ai.longdaoec.com`、数据盘 `/www`、完整迁移及旧域名兼容策略；优先完成本项目独立运行时和受限代理接入设计，保持 SEP 原运行时。时间先估算，演练后实测修订；付费测试没有专项预算时不执行。
2. 实施前重新检查两台机器的镜像、活跃色、部署文件、SEP 运行状态和资源，保存两边配置快照；现场一直在变化，不能只沿用本报告快照。
3. 按用户再次指示暂不排查、改动或继续追问上游；保留历史风险记录，不将上游连通或真实推理标为通过。环境准备、镜像传输和恢复演练单独推进；当前仅文档阶段，尚未执行正式放量。真实聊天、图片、视频和长 SSE 的验证另按指定预算执行。
4. 盘点支付服务商回调重试规则、OAuth 控制台回调列表、SMTP、历史媒体存储、来源 IP 白名单和所有旧客户端。Google 登录入口还需从实际用户网络验证；API 上游可达不代表 Google OAuth 可用。
5. 在线制作**演练用**数据库/文件/Redis 快照，在隔离环境恢复。该备份不是最终切流量的数据。演练实例不得收到公网业务流量、支付回调、队列任务或自动测试；不能连接生产 Redis/PG，避免恢复即产生重复业务。
6. 测量 dump、传输、restore、校验所需时间，用结果更新当前 10—30 分钟中转维护估算；用户要求暂不讨论具体时段。当前数据库约 1.3 GB 不等于最终压缩备份体积，不能据此保证停服分钟数。
7. 提前准备目标镜像、独立目录、空卷、专用数据库/Redis，准备 Caddy 最小配置差异、证书方案与回滚副本。首次正式恢复前不启动空库应用，防止自动初始化产生对象和管理员。
8. 新域名可先由现有 Caddy 提供维护页并完成证书准备；这是单独的生产配置动作，执行前审查具体差异。当前阶段没有做该动作。

恢复演练必须真的完成严格恢复与数据校验。`pg_restore -l` 仅验证归档目录可读，不等于恢复成功。

### 阶段 B：冻结、排空与最终快照

1. 记录窗口开始时间；禁止管理端充值、改余额、生成 Key、兑换码使用、注册及新推理请求等写入。对本项目入口返回可重试的维护响应；保留 SEP 自身页面服务。
2. 停止本项目定时测试、后台新任务及所有直接连接旧应用的消费者入口。核对不存在绕过 Caddy 访问旧应用的服务。
3. 排空在途 SSE、图片/视频提交及计费队列；待后台日志/账务刷盘完成，再向旧活跃实例优雅发送 SIGTERM。默认应用 drain 为 60 秒、容器 stop grace 为 90 秒；长请求未排空时延长窗口，不能强杀后宣称零丢失。
4. 核对旧实例确实退出、没有第二个应用实例/worker 在写 `sub2api` 或其 Redis 状态；检查支付未结订单、回调、额度脏集和账务积压。源码有停止 flusher 的逻辑，但执行时必须核对日志/结果，不能假定 stop 就一定刷完。
5. 支付回调在冻结窗口必须有明确处理：短时失败依赖服务商已确认的重试能力，或单独可靠暂存后重放；没有这项保证就先暂停新订单并核销未结订单，再开始窗口。
6. 取得最终 `sub2api` custom-format dump、应用文件归档、Redis RDB、私密配置及镜像标识。此后旧应用保持停止；**不停止旧共享 PostgreSQL、Redis、Caddy，也不停止旧 SEP 容器**。
7. 同一冻结边界生成数据库精确计数、余额与配置摘要，保存备份 SHA256，传输后两端复核。数据备份包含私密信息，目录权限 700、文件 600，不能入库。

下面仅示意最终备份的格式；正式命令还要加入确认后的私密备份目录、退出码检查及状态记录，本轮没有执行：

```bash
# 在旧机，且仅在本项目写入已冻结、后台已排空后
umask 077
docker exec longdao-postgres pg_dump -U sub2api -d sub2api -Fc > sub2api.dump
docker exec -i longdao-postgres pg_restore -l < sub2api.dump > sub2api.dump.list
# 文件卷应使用只读挂载/宿主机实际 Mountpoint 归档；不要重启已停的应用来打包。
# Redis 使用 redis-cli --rdb 取得新快照，不直接假定旧 dump.rdb 是最终时点。
```

单次 `pg_dump` 自身提供数据库一致快照，但不会阻止后续写入，也不会协调 Redis 与文件 [O1]，所以冻结步骤不能省略。

### 阶段 C：恢复及放量前校验

1. 只启动新建的龙道 AI PG，目标 Redis 保持停止；确认不是 `sep-postgres`/`sep-redis`，目标应用库为空，Redis 数据目录是本项目新建的干净目录。
2. 创建本应用角色/空库及必要扩展，恢复 custom dump，启用 `--exit-on-error`；建议使用 `--single-transaction`、`--no-owner`，并显式核对 owner/ACL，不盲目丢弃权限 [O2]。
3. 应用文件解压到正确父目录，避免产生 `/app/data/data`。恢复私密配置、密钥和 `.installed`，针对新网络调整 PG/Redis 主机与密码；同时核对 env 与 config.yaml 的有效值。
4. 向停止状态的独立 Redis 干净目录放入 RDB，以 `appendonly=no` 启动加载；核对 TTL、绑定、额度状态及锁的有效期。再在线启用 AOF，等待重写完成且状态正常，同步持久启动配置并在演练中验证重启恢复。已经自然到期的短期锁/令牌不能按数量不等就判错；不得批量删除不明 key 来“消除差异”。
5. 在应用启动前完成数据库关键摘要比对。新应用启动后监控/清理任务会产生合法变化，所以最终静态摘要应先比较。
6. 保持原镜像、原 schema，不把功能升级捆绑进迁移；检查 startup migration 行为及 schema_migrations，禁止未经评估引入新迁移。
7. 仅启动新 green 实例，限制访问，检查 `/health` 与日志。恢复后的实例会启动后台服务，因此从这一刻起即可能出现写入，进入后述“新库已写入”的回滚分支。
8. 更新业务 `settings` 中本站 API/前端地址、充值/邮件链接；逐项处理 Google/GitHub callback 和支付服务商配置。不能对数据库做全局旧域名字符串替换，历史订单、上游地址和审计内容需保留。
9. 合并目标 Caddy AI 站点，检查配置后 reload；Caddy 官方支持通过 reload 平滑应用配置，失败保留旧配置 [O5]。为防止 bind mount 指向旧 inode，文件写入方式必须与当前单文件挂载匹配，并复核容器读到的内容。

### 阶段 D：入口接管与兼容

正式 AI 域名当前已指向新 IP，因此重点是新站点、TLS 与业务放量，不能只把这次迁移叫作“改 DNS”。

按用户已确认的保留策略，旧机 `longdaoai.cn` 的 Caddy 站点改为 HTTPS 反向代理至新 AI 域名，设置正确的 upstream Host/TLS server name，保留流式 flush；旧应用继续停止，所有新旧入口都写新库。不要用 301/302 重定向替代 API POST 转发。检查跨代理真实 IP、限流和日志归属。

建议第一阶段维持新机 SEP 当前 `SUB2API_BASE_URL`，通过旧域名兼容入口接到新唯一后端，减少同时重启 SEP 的范围；之后再单独评估改成新域名或内网调用。这样会暂时多一跳，旧服务器仍是兼容入口依赖，不能立即销毁。

旧域名备案/接入状态未核实，本方案不安排其直接解析至大陆新机。浏览器在新域名通常需要重新登录；保留 JWT/Redis 有助于服务端状态延续，但不能搬迁浏览器的域名存储，也不保证旧 OAuth callback 可继续登录。

完成限定范围验收后再解除维护、开放普通流量。旧应用不能自动重启，旧机发布工作流也不能把它重新拉起；实施时需要检查并暂停该项目的自动部署入口，而非修改其他项目发布流程。

## 6. 数据与业务验收

| 类别 | 必须核对的证据 |
|---|---|
| 备份 | custom dump 目录可读、实际恢复成功、归档可解压、传输两端 SHA256 相同 |
| 完整库 | 冻结后的 80 张表实际清单及精确计数、schema、序列、索引、扩展、owner/ACL、迁移版本；不是只对照本轮九张表 |
| 资金 | 每位用户 ID、balance、frozen_balance、total_recharged 的逐行摘要及 decimal 汇总；API Key quota/quota_used、平台/订阅额度、订单及账务去重记录 |
| 密钥与配置 | 原 API Key/凭证只做私密确定性摘要；核对分组关联、倍率、价格、权限、settings、security_secrets 与有效 TOTP/支付密钥 |
| Redis | 本项目实际键、TTL/任务绑定/额度/刷新状态一致；不得因短期自然过期追求机械的全键数量相等 |
| TLS/页面 | 新域名证书链、SAN、HTTP→HTTPS、首页、管理员页面、health；旧入口如保留则同测 |
| 登录 | 密码登录、启用用户的 2FA、新域名刷新登录、Google/GitHub callback、密码重置/邮件验证链接 |
| 调用 | 原有 Key，无需重新发 Key；低成本真实推理、SSE 按块输出，长连接无代理缓冲 |
| 账务 | 指定测试 request_id 对应一次账务落库及正确扣费；重试不重复计费，不能只看总余额差 |
| 媒体 | 既有图片/视频任务查询与下载、历史媒体/对象外链；新增生成测试单列预算 |
| 支付 | 配置可解密、签名及回调可用、维护期订单补偿/重试完成、没有重复入账 |
| SEP | 原 SEP 页面/downloads/WebSocket 正常；中转调用成功；不以两个网站首页都能打开代替端到端验证 |
| 稳定性 | 错误率、上游超时、OOM/重启、磁盘、Redis 持久化、PG 连接及积压；观察期内保留旧机和备份 |

摘要结果放在私密运维目录，不把密码哈希、API Key、用户身份或原始账务明细写入仓库。最终计费测试会合法改变基线，要单独记录测试造成的变化。

## 7. 回滚规则

| 时点 | 允许的回滚 |
|---|---|
| 旧站未冻结 | 放弃准备，旧站继续；清理前确认目标产物归属 |
| 已冻结，且新库从未启动应用、没有任何新写入 | 恢复旧 Caddy 本项目路由，启动旧 green，核对健康/账务，再解除维护；SEP 共享设施保持运行 |
| 新应用已启动或已接收请求/回调 | 先冻结新写入并核对全部变化。不能直接启动旧账本；选择修复新端继续服务，或将最新 PG、Redis、文件及设置反向同步回旧端后切回 |
| 只是入口/TLS 配置出错 | 优先修复代理或恢复其上一个可用配置，继续使用唯一权威账本；共享 Caddy 回滚必须保留别的并行变更 |

反向同步仍须遵守共享设施边界：PG 只恢复本项目 `sub2api` 库，不覆盖旧 PostgreSQL 整个数据目录。**禁止用新端完整 RDB/Redis 卷覆盖旧共享 Redis**；应在旧端新增本项目专用 Redis 并让回滚应用连接它，或先验证归属，再按本应用键精确回迁并保留 TTL，不能覆盖 SEP 的 DB 1/2 或 DB 0 的未知队列键。该反向恢复也必须先演练。

RPO 目标为“成功结算业务无丢失”，依赖完整冻结、排空和最终快照确实成功。RTO 尚未测量，不能宣称已达到某个分钟数。后台任务、支付回调也算写入，不能用“还没有用户访问”作为可直接回旧库的判断条件。

## 8. 现有脚本的适用限制

- [backup.sh](../deploy/production/backup.sh) 只备份 PG SQL gzip 与 `/app/data`，缺少 Redis、env、镜像和入口配置，也没有跨组件冻结流程。
- [restore.sh](../deploy/production/restore.sh) 的 psql 导入没有设置 `ON_ERROR_STOP`，生产目标还跳过重建；存在 SQL 失败却返回成功、向已初始化库重复导入的风险。本次使用新空库及严格 pg_restore，不直接沿用该脚本完成迁移。
- [_active.sh](../deploy/production/_active.sh) 默认路径相对仓库根；活跃色缺失默认 blue，而现场是 green。必须设置绝对 COMPOSE_FILE/ENV_FILE/ACTIVE_COLOR_FILE，或统一执行目录。
- [docker-compose.yml](../deploy/production/docker-compose.yml) 包含固定容器/卷名、浮动数据库镜像及 Caddy 80/443 绑定；不能在新机不改就执行整栈 up。blue/green 同机切换也不能解决跨机器账本一致性。
- 本地已有用户未提交文件，本轮保留。后续镜像使用线上同一镜像，不把未提交工作区打包上线。

## 9. 证据来源与未完成验证

现场来源：R1 旧机 Docker inspect、镜像 inspect、文件元数据、`SHOW data_directory` 与只读 SQL；R2 新机同类检查和 Caddy admin API 的只读 GET；R3 公共 DNS JSON、指定 IP 的 curl 与两机无凭证 API 路径探测；R4 业务设置白名单字段与聚合统计。没有在文档保留秘密值。

官方资料（用于核对命令行为，不代替现场验证）：

- O1：PostgreSQL 18，pg_dump / SQL Dump。`https://www.postgresql.org/docs/18/app-pgdump.html`、`https://www.postgresql.org/docs/18/backup-dump.html`
- O2：PostgreSQL 18，pg_restore 的 exit-on-error / single-transaction。`https://www.postgresql.org/docs/18/app-pgrestore.html`
- O3：Redis CLI，Remote backups of RDB files。`https://redis.io/docs/latest/develop/tools/cli/`
- O4：Redis persistence，RDB/AOF 加载与备份。`https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/`
- O5：Caddy Command Line / reload。`https://caddyserver.com/docs/command-line`
- O6：Docker daemon data directory / containerd image store，说明 Docker `data-root` 与 containerd 根目录需分别处理。`https://docs.docker.com/engine/daemon/`、`https://docs.docker.com/engine/storage/containerd/`

尚未执行：备份传输、恢复演练、最终冻结对账、新站点证书配置、鉴权模型调用、付费推理/媒体测试、Google/GitHub 登录、真实邮件投递、支付回调、SEP 端到端验收、正式流量切换与回滚演练。以上均不可记为“通过”。
