# Binance + OKX DEX（BSC）篮子资金费套利机器人

这是一个 Go 编写的长期运行服务：通过 OKX DEX 聚合器在 BSC 买入现货，同时在 Binance USDⓈ-M 永续卖出近似等量合约，主要赚取正资金费。已有仓位出现可兑现的价差利润时，会先寻找收益更高且可用释放本金承接的候选；没有更好去处就继续持有，不会把一次实时费率下降直接当成换仓理由。

> 30% 年化只是策略目标或历史观察，不是保证收益。资金费率、链上滑点、合约风险、代币合约风险、RPC/交易所故障都可能造成亏损。首次部署只使用 `monitor`，随后至少用 `paper` 连续观察数周。

## 已实现的保护

- 默认 `monitor`，不下单；`paper` 使用实时行情模拟；`live` 需要配置、环境密钥和固定解锁口令同时满足。
- 仅允许配置中明确写入、确认过合约地址、且不是转账税币的代币。不能按 ticker 自动映射链上合约。
- 单币上限同时受总资金占比、固定上限、代币上限和实时链上深度限制。
- 深度不是看页面 TVL：机器人用 OKX DEX v6 聚合报价对目标金额逐级试单，并用 `depth_safety_multiplier` 留余量。
- 自动发现模式默认只监控每期资金费严格大于 `0.01%`（`1 bps`）的 Binance USDT 永续；先读取 OKX DEX 的 BSC 主流币目录，再通过 OKX 代币搜索补查目录外币种。只有 Binance 基础资产代码与 BSC 代币代码完全相同、且搜索结果只有一个合约地址时才视为匹配；同名多地址会跳过。所有 OKX 请求统一节流，遇到 HTTP 429 会退避重试。
- Binance 返回 HTTP 429/418 时，程序会读取 `Retry-After` 或错误正文中的封禁截止时间，在本地冷却窗口内停止全部 Binance HTTP 请求；资金费历史补录按每 750ms 一个请求节流，避免结算时集中请求。
- 链上深度和候选报价默认每 30 分钟刷新一次，每轮最多报价 40 个资金费候选；Binance 资金费行情仍按 `scan_seconds` 高频更新。深度不合格属于正常风控淘汰，不再记作系统故障。
- 程序启动时会按持仓中保存的合约地址恢复历史模拟/实盘仓位的代币配置，即使该币当前资金费已经低于新币筛选门槛，也不会向 OKX 发送空地址。
- 新开仓继续要求目标仓位的多倍买入深度；已有仓位改用当前真实持仓的 `代币→USDT` 卖出方向逐级探测。默认只有连续 3 次卖出容量不足才减仓，且每次减仓量不会超过已经验证可执行的卖出分块。所有分块均超过冲击阈值时只暂停并告警，不会把“容量为 0”解释成盲目全平。
- 实盘优先检查 Binance 爆仓距离。低于警戒线缓慢减仓，达到紧急线全部退出并告警。
- 自动纠正现货/空单数量偏差；候选扫描失败不会跳过已有仓位风险检查。
- 换仓必须同时满足：连续 3 次费率偏弱、最短持仓时间、新币经过入场确认、释放本金足以承接、新币 APR 优势、预计收益覆盖完整换仓成本。没有更好候选时继续持有当前仓位收取资金费。
- 价差利润按链上真实卖出报价、合约盈亏、资金费、手续费和 Gas 预留计算；只有另一个已确认候选在相同评估期内有足够的净收益优势，才会止盈并换仓。安全减仓不受此条件限制。
- OKX 返回的链、币种、数量、钱包、授权目标、授权 calldata、最小到账量和交易 calldata 都在本地复核；Gas limit 和 Gas price 另有硬上限。
- 每次开仓、减仓、对冲修正和平仓写入 `data/ledger.jsonl`；状态原子写入 `data/state.json`。
- 实盘双腿操作先写入 `pending_operations` 持久化日志；未完成操作、真实账户对账失败、行情过期或达到日亏损上限时，全局增险闸门会禁止开仓和换仓。
- 实盘启动会核对 Binance 空单、杠杆/保证金模式与 BSC 余额；升级不会清空原仓位，不一致时保持页面可查看但锁定交易。
- 旧版状态首次升级前会原样保存为 `state-v1-backup.json`；状态迁移失败会拒绝启动，不会用空仓状态覆盖原文件。
- 支持通用 Webhook 和 Telegram 告警。

## 本机先运行

1. 在 OKX Web3 开发者后台创建项目，取得 API Key、Secret、Passphrase 和 Project ID。监控和模拟报价也需要这四项凭据：

   ```powershell
   $env:FUNDING_BOT_OKX_API_KEY="..."
   $env:FUNDING_BOT_OKX_SECRET_KEY="..."
   $env:FUNDING_BOT_OKX_PASSPHRASE="..."
   $env:FUNDING_BOT_OKX_PROJECT_ID="..."
   ```

2. 复制配置：

   ```powershell
   Copy-Item config.example.json config.json
   ```

3. 保持 `mode` 为 `monitor`。配置中的 CAKE 和 WBNB 只是接口示例；要做 20–30 个币，必须逐个添加已经核对过的 BSC 合约地址与精度。OKX DEX 会自动选择和拆分链上路由。

4. 检查配置：

   ```powershell
   .\binance-bsc-funding-windows-amd64.exe -config config.json -check
   ```

5. 单独验证一个币的链上深度：

   ```powershell
   .\binance-bsc-funding-windows-amd64.exe -config config.json -probe CAKEUSDT
   ```

6. 启动监控：

   ```powershell
   .\binance-bsc-funding-windows-amd64.exe -config config.json
   ```

7. 浏览器打开：

   - `http://127.0.0.1:8788/`（手机适配的监控仪表盘）
   - `http://127.0.0.1:8788/healthz`
   - `http://127.0.0.1:8788/v1/funding`（全部正资金费率合约，包含是否匹配 BSC 现货）
   - `http://127.0.0.1:8788/v1/opportunities`
   - `http://127.0.0.1:8788/v1/positions`
   - `http://127.0.0.1:8788/v1/ledger`
   - `http://127.0.0.1:8788/v1/settings`（不包含密钥、钱包地址或实盘解锁字段）

确认长期稳定后，把 `mode` 改为 `paper`。模拟模式不需要 Binance API Key 或 BSC 私钥。当前本机 `config.json` 已切换为 `paper`；程序会用真实行情模拟开仓、减仓和平仓，并把结果写入页面的“模拟订单”标签。

## 手机查看仪表盘

仪表盘展示运行健康、全部正费率合约、BSC 匹配状态、套利候选、当前持仓和最近 200 条模拟/实盘订单流水。“参数设置”页可以修改策略、仓位、费率、深度、换仓和全仓风控参数。策略与风控参数在下一轮扫描边界生效；扫描间隔、杠杆、成本、代币发现和页面刷新参数保存后需要重启。

UI 修改保存在 `state_dir/ui-tuning.json`，启动时覆盖基础 `config.json` 中对应的非敏感调试参数。该文件不包含 API Key、私钥、钱包地址、节点 URL、代币合约白名单、运行模式或实盘解锁口令。每次修改都会写入带操作者身份的审计流水。若要完全回到基础配置，停止程序后删除 `ui-tuning.json` 再启动。

同一 Wi-Fi 下让手机访问时：

1. 在 `config.json` 把 `health_listen` 改成 `0.0.0.0:8788`，把 `dashboard.allow_lan` 改成 `true`；
2. 在启动程序的环境中设置 `FUNDING_BOT_DASHBOARD_PASSWORD`，不要写进 JSON；
3. 手机打开 `http://电脑局域网IP:8788/`，浏览器会要求用户名和密码，默认用户名为 `monitor`。

```powershell
$env:FUNDING_BOT_DASHBOARD_PASSWORD='请设置一个长随机密码'
```

管理员账号可查看和修改参数；可选的只读账号只能查看。连续认证失败会短时限速。局域网 HTTP 的 Basic Auth 只适合可信 Wi-Fi/VPN；不要在路由器映射 8788 端口，也不要直接把该端口暴露到公网。服务器远程查看应通过 Tailscale/WireGuard，或带 HTTPS 的反向代理。

## 自动发现与手工白名单

样例默认启用 `auto_discovery`，因此监控服务器不需要手工填写每个币。程序会：

1. 获取 Binance 所有正在交易的 USDT 永续；
2. 在 `/v1/funding` 展示全部当前正资金费率合约；
3. 获取 OKX DEX 的 BSC 主流币目录，并对目录未覆盖的正费率币调用代币搜索；
4. 符号完全相同且只有一个 BSC 地址时直接匹配；如果 API 返回多个同名地址，则结合 OKX 的社区/头部交易所认证、链上价格与流动性，只有唯一候选价格与 Binance 标记价相近时才匹配；
5. 对成功匹配的币继续检查蜜罐、交易税、真实往返报价、深度和净收益，并再次拒绝链上成交价与 Binance 标记价偏离超过 25% 的映射。

没有 BSC 现货或仍然无法唯一确定的同名币会继续监控资金费，但 `eligible_for_okx_quotes` 为 `false`，不会成为套利候选。监控页会区分“同名歧义”“无可信市场匹配”“搜索失败”等原因。自动发现只允许用于 `monitor` 和 `paper`；`live` 必须关闭它并使用人工审核过的白名单。

`tokens` 中的记录是可选手工白名单，也可作为自动目录不可用时的回退：

每个币都需要一条 `tokens` 记录：

```json
{
  "enabled": true,
  "symbol": "CAKE",
  "binance_symbol": "CAKEUSDT",
  "bsc_address": "0x0E09FaBB73Bd3Ade0a17ECC321fD13a19e81cE82",
  "decimals": 18,
  "max_notional_usdt": 500,
  "verified_contract": true,
  "fee_on_transfer": false
}
```

实盘白名单必须确认：

- Binance 上是正在交易的 USDT 永续合约；
- BSC 地址来自项目官网和区块浏览器的双重核对；
- token decimals 正确；
- 没有买卖税、黑名单、暂停交易、可增发或代理升级等不可接受风险；
- OKX DEX 能在 BSC 返回无交易税、非蜜罐且冲击合格的有效路由。没有路由或冲击过大会被自动排除。

## 关键参数

- `target_positions`: 目标持仓数量，样例为 25；实际最多只能达到已配置且通过筛选的币数。
- `auto_discovery.enabled`: 自动监控正费率合约并匹配 OKX DEX BSC 币表；仅允许监控和模拟。
- `auto_discovery.refresh_minutes`: 重新加载 OKX DEX BSC 币表的间隔。
- `dashboard.refresh_seconds`: 页面自动刷新间隔。
- `dashboard.allow_lan`: 是否允许非本机地址访问；启用时必须配置用户名和密码环境变量。
- `dashboard.read_only_username` / `read_only_password_env`: 可选只读账号；实盘模式始终要求管理员密码，即使服务仅监听回环地址。
- `strategy.max_entry_basis_bps`: BSC 买入价与 Binance 标记价的最大绝对入场基差；正基差还会作为潜在收敛损失计入回本和净年化。
- `risk.max_futures_margin_use_percent`: 最多使用多少比例的 Binance 可用保证金计算新增名义仓位。
- `risk.min_bsc_usdt_reserve`: BSC 钱包中不参与新开仓的 USDT 安全储备。
- `min_current_funding_bps_per_interval`: 当前这一期资金费率最低值。
- `min_funding_apr_percent`: 历史资金费 EWMA 的最低年化值。机器人会读取 Binance 调整过的结算周期，未返回时才使用 `funding_intervals_per_year`。
- `evaluation_hold_hours`: 用多长持有期估算收益能否覆盖完整往返磨损。
- `min_switch_apr_advantage_percent`: 新币相对旧币必须多出的年化收益。
- `target_notional_per_coin_usdt`: 单币目标名义金额。
- `max_capital_per_coin_percent`: 单币占总资金上限。
- `depth_safety_multiplier`: 例如 3 表示链上在 3 倍仓位下仍需通过冲击限制。
- `depth_reduction_trigger_percent`: 当前仓位超过安全容量多少百分比才视为深度违规，默认 10。
- `min_depth_reduction_percent`: 忽略小于该比例的微小减仓，默认 5。
- `depth_emergency_shortfall_percent`: 首次资金费结算前仍允许处理的紧急容量短缺比例，默认 50。
- `depth_breach_confirmations`: 连续多少次深度违规才执行，默认 3。
- `depth_close_cooldown_hours`: 因深度全部平仓后禁止重新开同一币种的小时数，默认 24。
- `min_liquidation_distance_x`: 空单爆仓价/标记价低于该倍数时开始减仓。
- `emergency_liquidation_distance_x`: 达到该线全部退出。
- `liquidation_breach_confirmations`: 普通单币爆仓距离违规需要连续确认的扫描次数，默认 3。
- `risk_reduce_cooldown_minutes`: 普通风险减仓后的组合冷却时间，默认 15 分钟；紧急风险不受冷却限制。
- `account_margin_stop_entry_percent`: 全仓保证金率达到该值时停止新开仓和换入，默认 55%。
- `account_margin_reduce_percent` / `account_margin_high_percent` / `account_margin_emergency_percent`: 账户级 10% / 25% / 50% 分级减仓线，默认 65% / 75% / 80%。每轮最多选择一个风险贡献最大的币。
- `max_hedge_drift_percent`: 两腿数量允许的最大偏差。
- `max_gas_limit` / `max_gas_price_gwei`: 单笔链上交易允许的硬上限，防止异常 API 响应签出过高 Gas 的交易。

杠杆档位本身不保证 3 倍爆仓距离。样例使用专用子账户的 `CROSSED` 保证金，必须在 Binance 留有足够的额外 USDT；机器人依据交易所返回的真实全仓爆仓价和账户保证金率减仓。普通三倍风险需要连续确认，紧急线和账户 80% 保证金率不等待确认。

## 实盘解锁

实盘前必须使用专用 Binance 子账户和专用 BSC 钱包。Binance API Key 只开合约交易权限，不开提现，不绑定不必要权限，并设置服务器 IP 白名单。BSC 钱包只放本策略需要的资金和少量 BNB Gas。

把密钥放进环境变量，不写入 JSON：

```powershell
$env:FUNDING_BOT_BINANCE_API_KEY="..."
$env:FUNDING_BOT_BINANCE_SECRET="..."
$env:FUNDING_BOT_BSC_PRIVATE_KEY="..."
$env:FUNDING_BOT_OKX_API_KEY="..."
$env:FUNDING_BOT_OKX_SECRET_KEY="..."
$env:FUNDING_BOT_OKX_PASSPHRASE="..."
$env:FUNDING_BOT_OKX_PROJECT_ID="..."
```

然后才把配置改为：

```json
"mode": "live",
"live_unlock": "I_UNDERSTAND_THIS_CAN_LOSE_MONEY"
```

实盘开仓顺序是链上现货先成交、Binance 空单后成交；每一阶段会先写入持久化操作日志。第二腿失败会立即尝试卖回现货，回滚交易哈希同样会持久化；无法确认回执时会锁定新增风险并发送严重告警。平仓/减仓先按 Binance 实际成交量买回空单，再卖出同比例 BSC 现货；链上交易尚未广播便失败时会恢复空单，广播后回执未知时不会盲目补偿，而是保留交易哈希并锁定交易等待对账。自动修正对冲偏差也使用确定性 Binance 订单号，重启后先查询真实仓位，不会盲目重复下单。

## Linux 服务器

仓库提供 `binance-bsc-funding.service`。推荐目录：

- 程序：`/opt/binance-bsc-funding/binance-bsc-funding`
- 配置：`/etc/binance-bsc-funding.json`
- 密钥：`/etc/binance-bsc-funding.env`，权限 `600`
- 状态：`/var/lib/binance-bsc-funding`

服务器配置要把 `state_dir` 改成 `/var/lib/binance-bsc-funding`。健康端口只监听 `127.0.0.1`，远程查看使用 SSH 端口转发，不要直接暴露到公网。

## 官方接口依据

- [Binance USDⓈ-M Futures API](https://developers.binance.com/en/docs/products/derivatives-trading-usds-futures/Introduction)
- [OKX DEX Swap API 介绍](https://web3.okx.com/onchainos/dev-docs/trade/dex-swap-api-introduction)
- [OKX DEX v6 报价接口](https://web3.okx.com/onchainos/dev-docs/trade/dex-get-quote)
- [OKX DEX v6 交易接口](https://web3.okx.com/onchainos/dev-docs/trade/dex-swap)
- [BNB Chain JSON-RPC](https://github.com/bnb-chain/bsc/blob/master/rpc/json-rpc-api.md)

机器人不把 OKX DEX 路由地址写死：授权目标、授权 calldata、交换目标和交换 calldata 每次都由官方 v6 API 返回，再经过本地严格校验后签名。这避免路由升级后继续向旧合约授权。
