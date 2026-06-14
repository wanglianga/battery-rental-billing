# 共享电池租借计费服务

基于 Go + Gin + PostgreSQL + Redis 构建的共享电池（共享充电宝）租借与计费核心服务。

## 原始需求

> 请开发共享电池租借计费服务，使用 Gin、PostgreSQL 和 Redis 维护电池柜、格口、电池编号、租借订单、押金、计费规则、归还记录、异常扣费和维修状态。用户扫码开柜租借电池，柜机上报门锁、电量、温度、锁舌、格口占用和离线补报；运营人员处理丢失、损坏、跨柜归还、账单争议和客服申诉。服务要能解释押金冻结、开柜成功、计费开始、归还识别、封顶扣费和退款释放每一步，面对重复扫码、设备断网、补报乱序和支付回调重放时不能多扣或漏扣。

## 技术栈

| 模块 | 技术 |
|------|------|
| Web 框架 | Gin v1.10 |
| 数据库 | PostgreSQL 16 + GORM |
| 缓存/锁 | Redis 7 + go-redis v9 |
| 认证 | JWT (HS256) |
| 密码哈希 | bcrypt |
| 部署 | Docker + Docker Compose |

## 核心业务能力

### 1. 资源管理
- **电池柜 (Cabinet)**：柜机编号、名称、地址、GPS、总格口、在线状态、心跳
- **格口 (Slot)**：柜机关联、格口号、电池绑定、占用状态、锁状态、预留过期
- **电池 (Battery)**：电池编号、型号、容量、SOC 电量、温度、循环次数、使用状态

### 2. 用户租借流程（防重复扫码）
1. 扫码 → 分布式锁 `lock:user_rent:{uid}` 防止并发重复下单
2. 校验：有无进行中订单、柜机是否在线、格口是否有可用电池
3. **押金冻结**：从用户余额扣减押金（`deposit_freeze` 流水），写入冻结状态
4. **开柜成功**：格口置为 `unlocking`，电池置为 `in_use`，生成解锁 token（Redis 5 分钟）
5. **计费开始**：订单从 `pending` → `renting`，记录起始时间与 SOC

### 3. 归还识别流程（防乱序与并发）
1. 柜机检测到电池插入 → `lock:return:{battery_no}` 分布式锁
2. **归还识别**：按电池号查找进行中订单，校验格口状态
3. **计费计算**：`CalcFee()` 实现首段免费 + 首段计费 + 阶梯计费 + 日封顶 + 总封顶
4. **封顶扣费**：当累计费用达 `MaxFee` 或日达 `DailyCap` 时，标记 `fee_cap_hit`
5. **退款释放**：从押金中抵扣费用，剩余押金释放退回用户余额
   - 费用 ≥ 押金：押金全抵扣
   - 费用 < 押金：先抵扣再退还差额（两条流水 `deduct` + `release`）

### 4. 柜机上报与离线补报
- **心跳**：更新柜机在线状态、心跳时间、固件版本
- **门锁上报**：锁结果成功时确认格口解锁
- **电池上报**：更新 SOC、温度、最后上报时间
- **格口批量上报**：同步各格口占用/门锁/电池信息，自动创建缺失格口
- **离线补报乱序处理**：
  - 每条上报按 `report_seq` 用 Redis `SETNX` 去重，TTL 30 天
  - 维护 `last_seq:{cabinet_no}`，**seq 小于已处理值的乱序报告只存不处理**
  - 批量补报自动按 seq 升序排序后逐条处理

### 5. 支付回调重放防护
- 回调唯一键 `pay_cb:{pay_no}:{third_txn}:{amount}` 用 Redis SETNX 防重放
- 支付单级别 `lock:pay_cb_lock:{pay_no}` 分布式锁
- 幂等校验：已 `success`/`refunded` 的订单直接返回不重复处理
- 金额一致性校验：回调金额与下单金额不符则置为 failed
- 所有回调内容（含重放）均记录 `callback_cnt` 和 `last_callback`

### 6. 运营管理
- **丢失处理 (MarkLost)**：计算基础租金 + 丢失赔偿费，押金全抵扣后如有余额退还
- **损坏处理 (MarkDamage)**：追加损坏赔偿金，电池标记为 `damaged`
- **人工归还 (ManualReturn)**：支持跨柜归还操作，完整走一遍计费结算
- **账单争议 (HandleDispute)**：支持全部/部分/驳回三种决策，调减费用并退款
- **维修工单**：柜机/格口/电池故障创建维修记录，状态流转 `pending→fixing→done/scrapped`

### 7. 幂等性保护
- 用户写操作接口支持 `X-Idempotent-Key` 请求头
- 相同 key + uid 在 TTL（默认 1 小时）内直接返回首次响应缓存
- 扫码租借、充值、回调等关键接口强制幂等

## 项目目录结构

```
wl-319/
├── cmd/server/main.go         # 入口
├── internal/
│   ├── api/router.go          # HTTP 路由与 handler
│   ├── auth/jwt.go            # JWT 签发/解析
│   ├── config/config.go       # 配置加载
│   ├── database/database.go   # PG 连接、迁移、种子
│   ├── handlers/
│   │   ├── rental_service.go  # 租借服务
│   │   ├── return_service.go  # 归还/结算服务
│   │   ├── report_service.go  # 柜机上报服务
│   │   └── op_service.go      # 运营/支付服务
│   ├── middleware/            # 中间件（认证、幂等、CORS、设备鉴权）
│   ├── models/models.go       # 全部数据模型
│   ├── redisx/redis.go        # Redis + 分布式锁
│   └── utils/utils.go         # 响应、计费算法、ID 生成
├── Dockerfile
├── docker-compose.yml
├── .dockerignore
├── .env.example
├── go.mod
└── README.md
```

## 启动方式

### 前置要求

- Docker ≥ 20.10
- Docker Compose v2
- 或本机运行需要：Go 1.22+, PostgreSQL 15+, Redis 7+

### 方式一：Docker Compose 一键启动（推荐）

#### 1. 一键构建并启动全部服务

```bash
docker compose up --build
```

后台运行：

```bash
docker compose up --build -d
```

启动后会依次启动：

1. **postgres** (端口 5432) — 自动建库建表 + 种子数据
2. **redis** (端口 6379) — 缓存与分布式锁
3. **server** (端口 8080) — 等待 PG/Redis 就绪后启动，自动迁移

#### 2. 查看日志与验证

```bash
# 查看所有服务日志
docker compose logs -f

# 单独看服务日志
docker compose logs -f server

# 健康检查
curl http://localhost:8080/api/v1/health
# 期望响应：{"code":0,"message":"ok","data":{"status":"ok","time":...}}
```

#### 3. 停止与清理

```bash
docker compose down

# 清理数据卷（谨慎！会删除所有数据库和Redis数据）
docker compose down -v
```

### 方式二：本机开发模式启动

#### 1. 准备 PostgreSQL 和 Redis

本机已有 PG 15+ 和 Redis 7+，或手动启动：

```bash
docker run -d --name br-pg -p 5432:5432 \
  -e POSTGRES_USER=battery -e POSTGRES_PASSWORD=battery123 -e POSTGRES_DB=battery_rental \
  postgres:16-alpine

docker run -d --name br-redis -p 6379:6379 redis:7-alpine
```

#### 2. 配置环境变量

```bash
cp .env.example .env
# 编辑 .env，将 POSTGRES_HOST / REDIS_HOST 改为 localhost
```

#### 3. 下载依赖并启动

```bash
go mod tidy
go run ./cmd/server
```

访问地址：**http://localhost:8080**

## 数据模型表清单（自动迁移）

| 表名 | 说明 | 关键字段 |
|------|------|---------|
| users | 用户 | phone, role(customer/admin/operator), balance, deposit_free |
| cabinets | 电池柜 | cabinet_no, status(online/offline/fault/maintenance), heartbeat_at |
| slots | 格口 | cabinet_id, slot_no, battery_id, status(empty/occupied/reserved/fault) |
| batteries | 电池 | battery_no, capacity, soc, temperature, status, cycle_count |
| billing_rules | 计费规则 | first_free_min, first_price, unit_price, daily_cap, max_fee, lost_fee |
| rental_orders | 租借订单 | order_no, status, deposit_amt, total_fee, start/end_time, cross_cabinet |
| deposit_records | 押金流水 | action(freeze/release/deduct/refund), before/after_bal, txn_id |
| return_records | 归还记录 | fee_calc, fee_cap_hit, cross_cabinet, detect/lock_time |
| exception_records | 异常扣费 | excep_type(lost/damage/overdue/cross_return), fee_amt, deposit_used |
| repair_records | 维修工单 | target_type(cabinet/slot/battery), status, fault_code, cost_amt |
| cabinet_reports | 柜机上报 | report_no, report_seq, report_type, device_time, is_replay, processed |
| dispute_records | 账单申诉 | status(open/reviewing/resolved/rejected), adjust_fee, refund_amt |
| payment_records | 支付单 | pay_no, third_txn_no, status, callback_cnt, raw_callback |
| idempotent_records | 幂等键记录 | key, request_id, action, payload, result, expire_at |

## 关键 API 速览

### 认证接口
```
POST /api/v1/auth/register          注册
POST /api/v1/auth/login             登录
POST /api/v1/auth/logout            登出
默认管理员：13800000000 / admin123
```

### 用户接口（Bearer Token）
```
GET  /api/v1/user/active-order      查询进行中订单
POST /api/v1/user/scan-rent         扫码开柜租借
GET  /api/v1/user/orders            我的订单列表
GET  /api/v1/user/orders/:id        订单详情（含押金流水、扣费明细）
POST /api/v1/user/dispute           提交账单申诉
```

### 支付接口
```
POST /api/v1/pay/recharge           充值（需登录）
GET  /api/v1/pay/mock/:pay_no       模拟支付成功（测试用）
POST /api/v1/pay/callback           第三方支付回调（幂等+重放防护）
```

### 柜机上报接口（X-Device-Token）
```
POST /api/v1/device/heartbeat       心跳上报
POST /api/v1/device/lock-report     门锁结果上报
POST /api/v1/device/battery-report  电池数据上报
POST /api/v1/device/slot-report     格口占用批量上报
POST /api/v1/device/offline-replay  离线补报（乱序自动处理）
POST /api/v1/device/detect-return   归还检测触发结算
```
设备 Token 规则：`cab-{cabinet_no}` 或固定值 `dev-{JWT前8位}`

### 运营管理接口（admin/operator 角色）
```
GET/POST /api/v1/admin/cabinets     柜机CRUD
GET/POST /api/v1/admin/batteries    电池CRUD + 分配到格口
GET/POST /api/v1/admin/rules        计费规则CRUD
POST /api/v1/admin/orders/manual-return   人工归还（跨柜归还用）
POST /api/v1/admin/orders/mark-lost       标记电池丢失
POST /api/v1/admin/orders/mark-damage     标记电池损坏
POST /api/v1/admin/disputes/handle        处理账单争议
GET/POST/PUT /api/v1/admin/repairs        维修工单管理
```

## 计费算法说明（utils.CalcFee）

```
输入参数：总时长(秒)、免费分钟、首段分钟、首段价格、阶梯分钟、阶梯价格、
         日封顶、最大天数、总封顶

1. 总分钟数 = ceil(秒数/60)
2. 如果 ≤ 免费分钟 → 0 元
3. 剩余分钟 = 总分钟 - 免费分钟
4. 整天计算：days = 剩余分钟 / 1440，如超过 max_days 则截断
5. 整日费用 = days × daily_cap，剩余分钟 = 剩余 - days×1440
6. 剩余零头计费：≤首段 → 首段价；超过部分按 unit_min 向上取整阶梯累计
7. 总费用 = 整日费 + 零头费
8. 若总费用 > max_fee → 取 max_fee 并标记 cap_hit=true
9. 若单日零头费 > daily_cap → 取 daily_cap 并标记 cap_hit=true
```

默认规则（种子数据）：前 5 分钟免费，首 30 分钟 2 元，之后每 30 分钟 1 元；
日封顶 30 元，总封顶 299 元；丢失赔偿 299 元，损坏赔偿 99 元。

## 防错与一致性保障清单

| 场景 | 机制 |
|------|------|
| 重复扫码下单 | `lock:user_rent:{uid}` + 进行中订单数校验 |
| 重复归还结算 | `lock:return:{battery_no}` + 订单状态校验 |
| 重复上报 | Redis `SETNX` 按 cabinet_no + report_seq 去重 |
| 补报乱序 | 维护 last_seq 游标，小于已处理 seq 的报告只入库不生效 |
| 回调重放 | `pay_cb:{pay_no}:{txn}:{amt}` 去重键 + 订单状态幂等校验 |
| 金额超扣 | 押金/余额更新必须走事务 + `SELECT ... FOR UPDATE` 行锁 |
| 并发库存 | 格口、电池分配使用数据库行级锁 + Redis 分布式锁双重保证 |
| 支付金额伪造 | 回调金额必须与下单金额严格一致，否则标记失败 |
| 接口幂等 | 写接口支持 X-Idempotent-Key，响应结果缓存并复用 |

## 默认初始账号

| 角色 | 手机号 | 密码 | 说明 |
|------|--------|------|------|
| 系统管理员 | 13800000000 | admin123 | 可访问全部管理接口 |

注册任意手机号（customer 角色）后调用 `/pay/mock/:pay_no` 模拟充值，即可用于完整租借流程测试。

## 快速冒烟测试流程

```bash
# 1. 注册普通用户
curl -X POST http://localhost:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"phone":"13900000001","password":"123456","nickname":"测试用户"}'

# 2. 登录拿 token
TOKEN=$(curl -s -X POST http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"phone":"13900000001","password":"123456"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["token"])')

# 3. 模拟充值 500 元（=50000 分，够冻结 299 押金）
PAY_NO=$(curl -s -X POST http://localhost:8080/api/v1/pay/recharge \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"amount":50000,"pay_type":"mock"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["pay_no"])')
curl "http://localhost:8080/api/v1/pay/mock/$PAY_NO"

# 4. 用管理员创建一个柜机 + 电池 + 分配
ADMIN_TOKEN=$(curl -s -X POST http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"phone":"13800000000","password":"admin123"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["token"])')

curl -X POST http://localhost:8080/api/v1/admin/cabinets \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"cabinet_no":"CAB001","name":"测试柜机A","address":"XX园区","total_slots":6}'

curl -X POST http://localhost:8080/api/v1/admin/batteries \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"battery_no":"BAT001","model":"PB-10K","capacity":10000,"soc":95}'

curl -X POST http://localhost:8080/api/v1/admin/batteries/assign \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"battery_no":"BAT001","cabinet_no":"CAB001","slot_no":1}'

# 5. 用户扫码租借
curl -X POST http://localhost:8080/api/v1/user/scan-rent \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"cabinet_no":"CAB001","slot_no":1}'

# 6. 设备归还检测（模拟 2 小时 = 7200s 后归还，用管理员改订单起始时间也可）
curl -X POST http://localhost:8080/api/v1/device/detect-return \
  -H 'X-Device-Token: cab-CAB001' -H 'Content-Type: application/json' \
  -d '{"cabinet_no":"CAB001","slot_no":2,"battery_no":"BAT001","soc":65,"temperature":28,"report_seq":1}'
```
