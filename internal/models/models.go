package models

import (
	"time"

	"gorm.io/gorm"
)

type BaseModel struct {
	ID        uint64         `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

type UserRole string

const (
	RoleCustomer UserRole = "customer"
	RoleAdmin    UserRole = "admin"
	RoleOperator UserRole = "operator"
)

type User struct {
	BaseModel
	Phone        string   `gorm:"size:20;uniqueIndex;not null" json:"phone"`
	Nickname     string   `gorm:"size:64" json:"nickname"`
	PasswordHash string   `gorm:"size:255" json:"-"`
	Role         UserRole `gorm:"size:20;default:customer;not null" json:"role"`
	Balance      int64    `gorm:"default:0;not null" json:"balance"`
	DepositFree  bool     `gorm:"default:false;not null" json:"deposit_free"`
	Status       int      `gorm:"default:1;not null" json:"status"`
}

type CabinetStatus string

const (
	CabinetOnline  CabinetStatus = "online"
	CabinetOffline CabinetStatus = "offline"
	CabinetFault   CabinetStatus = "fault"
	CabinetMaintain CabinetStatus = "maintenance"
)

type Cabinet struct {
	BaseModel
	CabinetNo    string        `gorm:"size:32;uniqueIndex;not null" json:"cabinet_no"`
	Name         string        `gorm:"size:128;not null" json:"name"`
	Address      string        `gorm:"size:255" json:"address"`
	Longitude    float64       `json:"longitude"`
	Latitude     float64       `json:"latitude"`
	TotalSlots   int           `gorm:"not null;default:12" json:"total_slots"`
	Status       CabinetStatus `gorm:"size:20;default:online;not null" json:"status"`
	LastOnlineAt *time.Time    `json:"last_online_at"`
	HeartbeatAt  *time.Time    `json:"heartbeat_at"`
	FirmwareVer  string        `gorm:"size:32" json:"firmware_ver"`
}

type SlotStatus string

const (
	SlotEmpty     SlotStatus = "empty"
	SlotOccupied  SlotStatus = "occupied"
	SlotReserved  SlotStatus = "reserved"
	SlotFault     SlotStatus = "fault"
	SlotUnlocking SlotStatus = "unlocking"
	SlotReturning SlotStatus = "returning"
)

type Slot struct {
	BaseModel
	CabinetID  uint64     `gorm:"not null;index" json:"cabinet_id"`
	SlotNo     int        `gorm:"not null" json:"slot_no"`
	BatteryID  *uint64    `gorm:"index" json:"battery_id"`
	Status     SlotStatus `gorm:"size:20;default:empty;not null" json:"status"`
	LockStatus int        `gorm:"default:1;not null" json:"lock_status"`
	LastAction *time.Time `json:"last_action"`
	ReserveExp *time.Time `json:"reserve_exp"`
	Cabinet    Cabinet    `gorm:"foreignKey:CabinetID" json:"-"`
	Battery    *Battery   `gorm:"foreignKey:BatteryID" json:"battery,omitempty"`
}

type BatteryStatus string

const (
	BatteryInCabinet BatteryStatus = "in_cabinet"
	BatteryInUse     BatteryStatus = "in_use"
	BatteryCharging  BatteryStatus = "charging"
	BatteryLost      BatteryStatus = "lost"
	BatteryDamaged   BatteryStatus = "damaged"
	BatteryRepair    BatteryStatus = "repair"
)

type Battery struct {
	BaseModel
	BatteryNo    string        `gorm:"size:32;uniqueIndex;not null" json:"battery_no"`
	Model        string        `gorm:"size:64" json:"model"`
	Capacity     int           `gorm:"not null;default:10000" json:"capacity"`
	SOC          int           `gorm:"default:0;not null" json:"soc"`
	Temperature  float64       `json:"temperature"`
	CycleCount   int           `gorm:"default:0" json:"cycle_count"`
	Status       BatteryStatus `gorm:"size:20;default:in_cabinet;not null" json:"status"`
	LastReportAt *time.Time    `json:"last_report_at"`
	SlotID       *uint64       `gorm:"index" json:"slot_id"`
	Slot         *Slot         `gorm:"foreignKey:SlotID" json:"-"`
}

type BillingRule struct {
	BaseModel
	Name           string `gorm:"size:64;not null" json:"name"`
	FirstFreeMin   int    `gorm:"default:0;not null" json:"first_free_min"`
	FirstPeriodMin int    `gorm:"default:30;not null" json:"first_period_min"`
	FirstPrice     int64  `gorm:"default:200;not null" json:"first_price"`
	UnitMin        int    `gorm:"default:30;not null" json:"unit_min"`
	UnitPrice      int64  `gorm:"default:100;not null" json:"unit_price"`
	DailyCap       int64  `gorm:"default:3000;not null" json:"daily_cap"`
	MaxDays        int    `gorm:"default:30;not null" json:"max_days"`
	MaxFee         int64  `gorm:"default:0;not null" json:"max_fee"`
	LostFee        int64  `gorm:"default:29900;not null" json:"lost_fee"`
	DamageFee      int64  `gorm:"default:9900;not null" json:"damage_fee"`
	IsDefault      bool   `gorm:"default:false;not null" json:"is_default"`
	Active         bool   `gorm:"default:true;not null" json:"active"`
}

type OrderStatus string

const (
	OrderPending    OrderStatus = "pending"
	OrderRenting    OrderStatus = "renting"
	OrderReturning  OrderStatus = "returning"
	OrderCompleted  OrderStatus = "completed"
	OrderLost       OrderStatus = "lost"
	OrderDisputed   OrderStatus = "disputed"
	OrderCancelled  OrderStatus = "cancelled"
)

type RentalOrder struct {
	BaseModel
	OrderNo       string      `gorm:"size:32;uniqueIndex;not null" json:"order_no"`
	UserID        uint64      `gorm:"not null;index" json:"user_id"`
	BatteryID     uint64      `gorm:"not null;index" json:"battery_id"`
	FromCabinetID uint64      `gorm:"not null;index" json:"from_cabinet_id"`
	FromSlotID    uint64      `gorm:"not null;index" json:"from_slot_id"`
	ToCabinetID   *uint64     `gorm:"index" json:"to_cabinet_id"`
	ToSlotID      *uint64     `gorm:"index" json:"to_slot_id"`
	Status        OrderStatus `gorm:"size:20;default:pending;not null;index" json:"status"`
	RuleID        uint64      `gorm:"not null" json:"rule_id"`
	DepositAmt    int64       `gorm:"not null;default:0" json:"deposit_amt"`
	DepositStatus int         `gorm:"default:0;not null" json:"deposit_status"`
	PayTxnID      *string     `gorm:"size:64;index" json:"pay_txn_id"`
	TotalFee      int64       `gorm:"default:0;not null" json:"total_fee"`
	PaidFee       int64       `gorm:"default:0;not null" json:"paid_fee"`
	ExceptionFee  int64       `gorm:"default:0;not null" json:"exception_fee"`
	RefundAmt     int64       `gorm:"default:0;not null" json:"refund_amt"`
	StartTime     *time.Time  `json:"start_time"`
	EndTime       *time.Time  `json:"end_time"`
	DurationSec   int64       `gorm:"default:0" json:"duration_sec"`
	StartSOC      int         `gorm:"default:0" json:"start_soc"`
	EndSOC        int         `gorm:"default:0" json:"end_soc"`
	CrossCabinet  bool        `gorm:"default:false;not null" json:"cross_cabinet"`
	Remarks       string      `gorm:"size:512" json:"remarks"`
	User          User        `gorm:"foreignKey:UserID" json:"-"`
	Battery       Battery     `gorm:"foreignKey:BatteryID" json:"battery,omitempty"`
	Rule          BillingRule `gorm:"foreignKey:RuleID" json:"rule,omitempty"`
}

type DepositAction string

const (
	DepositFreeze  DepositAction = "freeze"
	DepositRelease DepositAction = "release"
	DepositDeduct  DepositAction = "deduct"
	DepositRefund  DepositAction = "refund"
)

type DepositRecord struct {
	BaseModel
	OrderID    uint64        `gorm:"not null;index" json:"order_id"`
	UserID     uint64        `gorm:"not null;index" json:"user_id"`
	Action     DepositAction `gorm:"size:20;not null" json:"action"`
	Amount     int64         `gorm:"not null" json:"amount"`
	BeforeBal  int64         `gorm:"default:0" json:"before_bal"`
	AfterBal   int64         `gorm:"default:0" json:"after_bal"`
	TxnID      string        `gorm:"size:64;index" json:"txn_id"`
	Status     int           `gorm:"default:1;not null" json:"status"`
	Reason     string        `gorm:"size:256" json:"reason"`
	ProcessedAt *time.Time   `json:"processed_at"`
	Order      RentalOrder   `gorm:"foreignKey:OrderID" json:"-"`
	User       User          `gorm:"foreignKey:UserID" json:"-"`
}

type ReturnRecord struct {
	BaseModel
	OrderID     uint64     `gorm:"not null;uniqueIndex" json:"order_id"`
	CabinetID   uint64     `gorm:"not null;index" json:"cabinet_id"`
	SlotID      uint64     `gorm:"not null;index" json:"slot_id"`
	BatteryID   uint64     `gorm:"not null;index" json:"battery_id"`
	UserID      uint64     `gorm:"not null;index" json:"user_id"`
	DetectTime  time.Time  `gorm:"not null" json:"detect_time"`
	LockTime    *time.Time `json:"lock_time"`
	SOC         int        `gorm:"default:0" json:"soc"`
	Temperature float64    `json:"temperature"`
	CrossCabinet bool       `gorm:"default:false;not null" json:"cross_cabinet"`
	FeeCalc     int64      `gorm:"default:0" json:"fee_calc"`
	FeeCapHit   bool       `gorm:"default:false;not null" json:"fee_cap_hit"`
	Remarks     string     `gorm:"size:256" json:"remarks"`
	Order       RentalOrder `gorm:"foreignKey:OrderID" json:"-"`
}

type ExceptionType string

const (
	ExcepLost        ExceptionType = "lost"
	ExcepDamage      ExceptionType = "damage"
	ExcepOverdue     ExceptionType = "overdue"
	ExcepCrossReturn ExceptionType = "cross_return"
	ExcepManual      ExceptionType = "manual"
	ExcepDeviceFault ExceptionType = "device_fault"
)

type ExceptionRecord struct {
	BaseModel
	OrderID       uint64        `gorm:"index" json:"order_id"`
	UserID        uint64        `gorm:"not null;index" json:"user_id"`
	BatteryID     uint64        `gorm:"index" json:"battery_id"`
	ExcepType     ExceptionType `gorm:"size:32;not null" json:"excep_type"`
	FeeAmt        int64         `gorm:"default:0;not null" json:"fee_amt"`
	FeeDeducted   int64         `gorm:"default:0;not null" json:"fee_deducted"`
	DepositUsed   int64         `gorm:"default:0;not null" json:"deposit_used"`
	RefundAmt     int64         `gorm:"default:0;not null" json:"refund_amt"`
	Status        int           `gorm:"default:0;not null" json:"status"`
	HandledBy     *uint64       `gorm:"index" json:"handled_by"`
	HandledAt     *time.Time    `json:"handled_at"`
	Evidence      string        `gorm:"size:512" json:"evidence"`
	Description   string        `gorm:"size:1024" json:"description"`
	Order         RentalOrder   `gorm:"foreignKey:OrderID" json:"-"`
}

type RepairStatus string

const (
	RepairPending  RepairStatus = "pending"
	RepairFixing   RepairStatus = "fixing"
	RepairDone     RepairStatus = "done"
	RepairScrapped RepairStatus = "scrapped"
)

type RepairRecord struct {
	BaseModel
	TargetType  string       `gorm:"size:16;not null" json:"target_type"`
	TargetID    uint64       `gorm:"not null;index" json:"target_id"`
	ReportBy    *uint64      `gorm:"index" json:"report_by"`
	RepairBy    *uint64      `gorm:"index" json:"repair_by"`
	Status      RepairStatus `gorm:"size:20;default:pending;not null;index" json:"status"`
	FaultCode   string       `gorm:"size:32" json:"fault_code"`
	Description string       `gorm:"size:1024" json:"description"`
	CostAmt     int64        `gorm:"default:0" json:"cost_amt"`
	StartAt     *time.Time   `json:"start_at"`
	FinishAt    *time.Time   `json:"finish_at"`
	Remarks     string       `gorm:"size:512" json:"remarks"`
}

type ReportType string

const (
	ReportHeartbeat  ReportType = "heartbeat"
	ReportLock       ReportType = "lock"
	ReportBattery    ReportType = "battery"
	ReportSlot       ReportType = "slot"
	ReportDoor       ReportType = "door"
	ReportAlarm      ReportType = "alarm"
	ReportOffline    ReportType = "offline_replay"
)

type CabinetReport struct {
	BaseModel
	CabinetID   uint64     `gorm:"not null;index" json:"cabinet_id"`
	ReportNo    string     `gorm:"size:64;not null;uniqueIndex" json:"report_no"`
	ReportSeq   int64      `gorm:"default:0;index" json:"report_seq"`
	ReportType  ReportType `gorm:"size:32;not null;index" json:"report_type"`
	DeviceTime  time.Time  `gorm:"not null" json:"device_time"`
	ServerTime  time.Time  `gorm:"not null" json:"server_time"`
	IsReplay    bool       `gorm:"default:false;not null" json:"is_replay"`
	Processed   bool       `gorm:"default:false;index" json:"processed"`
	ProcessedAt *time.Time `json:"processed_at"`
	Payload     string     `gorm:"type:text" json:"payload"`
	SlotNo      *int       `gorm:"index" json:"slot_no"`
	BatteryNo   *string    `gorm:"size:32;index" json:"battery_no"`
	Cabinet     Cabinet    `gorm:"foreignKey:CabinetID" json:"-"`
}

type DisputeStatus string

const (
	DisputeOpen     DisputeStatus = "open"
	DisputeReview   DisputeStatus = "reviewing"
	DisputeResolved DisputeStatus = "resolved"
	DisputeRejected DisputeStatus = "rejected"
)

type DisputeRecord struct {
	BaseModel
	OrderID      uint64        `gorm:"not null;index" json:"order_id"`
	UserID       uint64        `gorm:"not null;index" json:"user_id"`
	Title        string        `gorm:"size:256;not null" json:"title"`
	Content      string        `gorm:"type:text;not null" json:"content"`
	Status       DisputeStatus `gorm:"size:20;default:open;not null;index" json:"status"`
	FiledFee     int64         `gorm:"default:0" json:"filed_fee"`
	AdjustFee    int64         `gorm:"default:0" json:"adjust_fee"`
	RefundAmt    int64         `gorm:"default:0" json:"refund_amt"`
	HandledBy    *uint64       `gorm:"index" json:"handled_by"`
	HandledAt    *time.Time    `json:"handled_at"`
	ReplyContent string        `gorm:"type:text" json:"reply_content"`
	Order        RentalOrder   `gorm:"foreignKey:OrderID" json:"-"`
}

type PayStatus string

const (
	PayInit     PayStatus = "init"
	PayPending  PayStatus = "pending"
	PaySuccess  PayStatus = "success"
	PayFailed   PayStatus = "failed"
	PayRefunding PayStatus = "refunding"
	PayRefunded PayStatus = "refunded"
)

type PaymentRecord struct {
	BaseModel
	PayNo       string    `gorm:"size:64;uniqueIndex;not null" json:"pay_no"`
	OrderID     uint64    `gorm:"not null;index" json:"order_id"`
	UserID      uint64    `gorm:"not null;index" json:"user_id"`
	PayType     string    `gorm:"size:16;not null" json:"pay_type"`
	Amount      int64     `gorm:"not null" json:"amount"`
	Status      PayStatus `gorm:"size:20;default:init;not null;index" json:"status"`
	ThirdTxnNo  *string   `gorm:"size:128;index" json:"third_txn_no"`
	Subject     string    `gorm:"size:256" json:"subject"`
	ExpireAt    *time.Time `json:"expire_at"`
	PaidAt      *time.Time `json:"paid_at"`
	CallbackCnt int       `gorm:"default:0" json:"callback_cnt"`
	LastCallback *time.Time `json:"last_callback"`
	RawCallback string    `gorm:"type:text" json:"raw_callback"`
	Order       RentalOrder `gorm:"foreignKey:OrderID" json:"-"`
}

type IdempotentRecord struct {
	BaseModel
	Key       string    `gorm:"size:128;uniqueIndex;not null" json:"key"`
	RequestID string    `gorm:"size:64;index" json:"request_id"`
	UserID    uint64    `gorm:"index" json:"user_id"`
	Action    string    `gorm:"size:64;not null" json:"action"`
	Payload   string    `gorm:"type:text" json:"payload"`
	Result    string    `gorm:"type:text" json:"result"`
	ExpireAt  time.Time `gorm:"not null;index" json:"expire_at"`
}
