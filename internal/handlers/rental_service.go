package handlers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"battery-rental/internal/config"
	"battery-rental/internal/database"
	"battery-rental/internal/models"
	"battery-rental/internal/redisx"
	"battery-rental/internal/utils"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type RentalService struct{}

func NewRentalService() *RentalService {
	return &RentalService{}
}

type ScanRentalReq struct {
	CabinetNo string `json:"cabinet_no" binding:"required"`
	SlotNo    *int   `json:"slot_no"`
}

type ScanRentalResp struct {
	OrderNo      string    `json:"order_no"`
	CabinetNo    string    `json:"cabinet_no"`
	SlotNo       int       `json:"slot_no"`
	BatteryNo    string    `json:"battery_no"`
	BatterySOC   int       `json:"battery_soc"`
	DepositAmt   int64     `json:"deposit_amt"`
	DepositFree  bool      `json:"deposit_free"`
	LockOpen     bool      `json:"lock_open"`
	StartTime    time.Time `json:"start_time"`
	Status       string    `json:"status"`
	UnlockToken  string    `json:"unlock_token,omitempty"`
}

func (s *RentalService) ScanAndRent(ctx context.Context, userID uint64, req *ScanRentalReq) (*ScanRentalResp, error) {
	lockKey := fmt.Sprintf("user_rent:%d", userID)
	lock, acquired, err := redisx.AcquireLock(ctx, lockKey, 10*time.Second, 3)
	if err != nil {
		return nil, fmt.Errorf("系统繁忙，请稍后再试")
	}
	if !acquired {
		return nil, fmt.Errorf("操作太频繁，请稍后再试")
	}
	defer lock.Release(ctx)

	var existingActive int64
	database.DB.Model(&models.RentalOrder{}).
		Where("user_id = ? AND status IN ?", userID, []models.OrderStatus{
			models.OrderPending, models.OrderRenting, models.OrderReturning,
		}).Count(&existingActive)
	if existingActive > 0 {
		return nil, fmt.Errorf("您有进行中的订单，请先归还电池")
	}

	var cabinet models.Cabinet
	if err := database.DB.Where("cabinet_no = ?", req.CabinetNo).First(&cabinet).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("柜机不存在")
		}
		return nil, fmt.Errorf("查询柜机失败")
	}
	if cabinet.Status != models.CabinetOnline {
		return nil, fmt.Errorf("柜机当前不可用，状态：%s", cabinet.Status)
	}

	var user models.User
	if err := database.DB.First(&user, userID).Error; err != nil {
		return nil, fmt.Errorf("用户信息不存在")
	}
	if user.Status != 1 {
		return nil, fmt.Errorf("账户已被冻结")
	}

	var slot models.Slot
	tx := database.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	if req.SlotNo != nil {
		if err := tx.Clauses(clauseUpdateLock).Where("cabinet_id = ? AND slot_no = ?", cabinet.ID, *req.SlotNo).
			First(&slot).Error; err != nil {
			tx.Rollback()
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, fmt.Errorf("指定格口不存在")
			}
			return nil, fmt.Errorf("查询格口失败")
		}
		if slot.Status != models.SlotOccupied || slot.BatteryID == nil {
			tx.Rollback()
			return nil, fmt.Errorf("指定格口无可用电池")
		}
	} else {
		if err := tx.Clauses(clauseUpdateLock).Where("cabinet_id = ? AND status = ?", cabinet.ID, models.SlotOccupied).
			Order("id ASC").Limit(1).First(&slot).Error; err != nil {
			tx.Rollback()
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, fmt.Errorf("该柜机暂无可用电池")
			}
			return nil, fmt.Errorf("查询可用格口失败")
		}
	}

	var battery models.Battery
	if err := tx.Clauses(clauseUpdateLock).First(&battery, *slot.BatteryID).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("查询电池失败")
	}
	if battery.Status != models.BatteryInCabinet {
		tx.Rollback()
		return nil, fmt.Errorf("电池状态异常")
	}
	if battery.SOC < 20 {
		tx.Rollback()
		return nil, fmt.Errorf("该电池电量过低，请选择其他电池")
	}

	var rule models.BillingRule
	if err := tx.Where("is_default = ? AND active = ?", true, true).First(&rule).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("计费规则未配置")
	}

	depositAmt := int64(0)
	depositFree := user.DepositFree
	if !depositFree {
		depositAmt = config.AppConfig.DepositAmt
		if user.Balance < depositAmt {
			tx.Rollback()
			return nil, fmt.Errorf("余额不足，请先充值。押金需%d分", depositAmt)
		}
	}

	now := time.Now()
	orderNo := utils.GenOrderNo()
	txnID := utils.GenTxnID()

	order := models.RentalOrder{
		OrderNo:       orderNo,
		UserID:        userID,
		BatteryID:     battery.ID,
		FromCabinetID: cabinet.ID,
		FromSlotID:    slot.ID,
		Status:        models.OrderPending,
		RuleID:        rule.ID,
		DepositAmt:    depositAmt,
		DepositStatus: 0,
		TotalFee:      0,
		StartSOC:      battery.SOC,
		StartTime:     &now,
		Remarks:       fmt.Sprintf("扫码开柜：柜机%s格口%d", cabinet.CabinetNo, slot.SlotNo),
	}
	if err := tx.Create(&order).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("创建订单失败")
	}

	if depositAmt > 0 {
		beforeBal := user.Balance
		afterBal := user.Balance - depositAmt
		depRec := models.DepositRecord{
			OrderID:     order.ID,
			UserID:      userID,
			Action:      models.DepositFreeze,
			Amount:      depositAmt,
			BeforeBal:   beforeBal,
			AfterBal:    afterBal,
			TxnID:       txnID,
			Status:      1,
			Reason:      fmt.Sprintf("租借电池押金冻结：订单%s", orderNo),
			ProcessedAt: &now,
		}
		if err := tx.Create(&depRec).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("创建押金记录失败")
		}
		if err := tx.Model(&user).Update("balance", afterBal).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("冻结押金失败")
		}
		order.DepositStatus = 1
	}

	if err := tx.Model(&order).Updates(map[string]interface{}{
		"deposit_status": order.DepositStatus,
		"status":         models.OrderRenting,
	}).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("更新订单状态失败")
	}

	slot.Status = models.SlotUnlocking
	slot.LastAction = &now
	if err := tx.Save(&slot).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("更新格口状态失败")
	}

	battery.Status = models.BatteryInUse
	battery.SlotID = nil
	battery.LastReportAt = &now
	if err := tx.Save(&battery).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("更新电池状态失败")
	}

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("提交事务失败")
	}

	unlockToken := utils.GenTxnID()
	_ = redisx.SetEX(ctx, fmt.Sprintf("unlock:%s:%d", cabinet.CabinetNo, slot.SlotNo), unlockToken, 5*time.Minute)
	_ = redisx.SetEX(ctx, fmt.Sprintf("order:slot:%d", slot.ID), order.ID, 24*time.Hour)

	return &ScanRentalResp{
		OrderNo:     orderNo,
		CabinetNo:   cabinet.CabinetNo,
		SlotNo:      slot.SlotNo,
		BatteryNo:   battery.BatteryNo,
		BatterySOC:  battery.SOC,
		DepositAmt:  depositAmt,
		DepositFree: depositFree,
		LockOpen:    true,
		StartTime:   now,
		Status:      string(models.OrderRenting),
		UnlockToken: unlockToken,
	}, nil
}

func (s *RentalService) ConfirmUnlock(ctx context.Context, cabinetNo string, slotNo int, success bool) error {
	var cabinet models.Cabinet
	if err := database.DB.Where("cabinet_no = ?", cabinetNo).First(&cabinet).Error; err != nil {
		return fmt.Errorf("柜机不存在")
	}
	var slot models.Slot
	if err := database.DB.Where("cabinet_id = ? AND slot_no = ?", cabinet.ID, slotNo).First(&slot).Error; err != nil {
		return fmt.Errorf("格口不存在")
	}
	now := time.Now()
	if success {
		slot.Status = models.SlotEmpty
		slot.BatteryID = nil
		slot.LockStatus = 1
	} else {
		slot.Status = models.SlotFault
	}
	slot.LastAction = &now
	database.DB.Save(&slot)
	return nil
}

func LockUpdate() interface{} {
	type locker struct{}
	return locker{}
}

var clauseUpdateLock = clause.Locking{Strength: "UPDATE"}

var _ = LockUpdate
