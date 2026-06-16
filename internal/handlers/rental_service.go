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

var clauseUpdateLock = clause.Locking{Strength: "UPDATE"}

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
	IsAbnormal   bool      `json:"is_abnormal,omitempty"`
	AbnormalReason string  `json:"abnormal_reason,omitempty"`
	CompensationAction string `json:"compensation_action,omitempty"`
}

type ReportUnlockFailureReq struct {
	OrderNo string `json:"order_no" binding:"required"`
	Reason  string `json:"reason" binding:"required"`
	Evidence string `json:"evidence"`
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

	now := time.Now()
	if cabinet.Status != models.CabinetOnline {
		abnResp, abnErr := s.createAbnormalOrder(ctx, userID, &cabinet, req.SlotNo, models.AbnormalReasonCabinetOffline)
		if abnErr == nil && abnResp != nil {
			return abnResp, nil
		}
		return nil, fmt.Errorf("柜机当前不可用，状态：%s", cabinet.Status)
	}
	if cabinet.LastOnlineAt != nil && now.Sub(*cabinet.LastOnlineAt) > 5*time.Minute {
		_ = database.DB.Model(&cabinet).Update("status", models.CabinetOffline)
		abnResp, abnErr := s.createAbnormalOrder(ctx, userID, &cabinet, req.SlotNo, models.AbnormalReasonCabinetOffline)
		if abnErr == nil && abnResp != nil {
			return abnResp, nil
		}
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
		if slot.Status == models.SlotFault {
			tx.Rollback()
			abnResp, abnErr := s.createAbnormalOrderForSlot(ctx, userID, &cabinet, slot.SlotNo, models.AbnormalReasonLockStuck)
			if abnErr == nil && abnResp != nil {
				return abnResp, nil
			}
			return nil, fmt.Errorf("指定格口故障，状态：%s", slot.Status)
		}
		if slot.Status != models.SlotOccupied || slot.BatteryID == nil {
			tx.Rollback()
			abnResp, abnErr := s.createAbnormalOrderForSlot(ctx, userID, &cabinet, slot.SlotNo, models.AbnormalReasonBatteryMissing)
			if abnErr == nil && abnResp != nil {
				return abnResp, nil
			}
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
	batteryFound := false
	if slot.BatteryID != nil {
		if err := tx.Clauses(clauseUpdateLock).First(&battery, *slot.BatteryID).Error; err == nil {
			batteryFound = true
		}
	}
	if !batteryFound || battery.ID == 0 {
		tx.Rollback()
		abnResp, abnErr := s.createAbnormalOrderForSlot(ctx, userID, &cabinet, slot.SlotNo, models.AbnormalReasonBatteryMissing)
		if abnErr == nil && abnResp != nil {
			return abnResp, nil
		}
		return nil, fmt.Errorf("格口无有效电池，电池实际缺失")
	}
	if battery.Status != models.BatteryInCabinet {
		tx.Rollback()
		_ = s.markSlotAbnormal(ctx, userID, &cabinet, &slot, &battery, models.AbnormalReasonBatteryMissing)
		abnResp, abnErr := s.createAbnormalOrderForSlot(ctx, userID, &cabinet, slot.SlotNo, models.AbnormalReasonBatteryMissing)
		if abnErr == nil && abnResp != nil {
			return abnResp, nil
		}
		return nil, fmt.Errorf("电池状态异常")
	}
	if battery.SOC < 20 {
		tx.Rollback()
		_ = s.markSlotAbnormal(ctx, userID, &cabinet, &slot, &battery, models.AbnormalReasonBatteryMissing)
		abnResp, abnErr := s.createAbnormalOrderForSlot(ctx, userID, &cabinet, slot.SlotNo, models.AbnormalReasonBatteryMissing)
		if abnErr == nil && abnResp != nil {
			return abnResp, nil
		}
		return nil, fmt.Errorf("该电池电量过低，请选择其他电池")
	}
	if slot.LockStatus != 1 {
		tx.Rollback()
		abnResp, abnErr := s.createAbnormalOrderForSlot(ctx, userID, &cabinet, slot.SlotNo, models.AbnormalReasonLockStuck)
		if abnErr == nil && abnResp != nil {
			return abnResp, nil
		}
		return nil, fmt.Errorf("格门锁异常，锁舌卡住")
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



func (s *RentalService) createAbnormalOrder(ctx context.Context, userID uint64, cabinet *models.Cabinet, slotNo *int, reason models.AbnormalReason) (*ScanRentalResp, error) {
	var user models.User
	if err := database.DB.First(&user, userID).Error; err != nil {
		return nil, fmt.Errorf("用户信息不存在")
	}
	if user.Status != 1 {
		return nil, fmt.Errorf("账户已被冻结")
	}

	var slot models.Slot
	selectedSlotNo := 0
	tx := database.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	if slotNo != nil {
		selectedSlotNo = *slotNo
		if err := tx.Where("cabinet_id = ? AND slot_no = ?", cabinet.ID, *slotNo).First(&slot).Error; err != nil {
			selectedSlotNo = 0
		}
	}
	if selectedSlotNo == 0 {
		if err := tx.Where("cabinet_id = ? AND status = ?", cabinet.ID, models.SlotOccupied).
			Order("id ASC").Limit(1).First(&slot).Error; err == nil {
			selectedSlotNo = slot.SlotNo
		}
	}
	if selectedSlotNo == 0 {
		tx.Rollback()
		return nil, fmt.Errorf("无可用格口信息")
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
			return nil, fmt.Errorf("余额不足，押金需%d分", depositAmt)
		}
	}

	now := time.Now()
	orderNo := utils.GenOrderNo()
	txnID := utils.GenTxnID()

	batteryID := uint64(0)
	batteryNo := ""
	batterySOC := 0
	if slot.BatteryID != nil {
		var battery models.Battery
		if err := tx.First(&battery, *slot.BatteryID).Error; err == nil {
			batteryID = battery.ID
			batteryNo = battery.BatteryNo
			batterySOC = battery.SOC
		}
	}

	order := models.RentalOrder{
		OrderNo:            orderNo,
		UserID:             userID,
		BatteryID:          batteryID,
		FromCabinetID:      cabinet.ID,
		FromSlotID:         slot.ID,
		Status:             models.OrderAbnormal,
		RuleID:             rule.ID,
		DepositAmt:         depositAmt,
		DepositStatus:      0,
		TotalFee:           0,
		StartSOC:           batterySOC,
		StartTime:          nil,
		BillingEnabled:     false,
		AbnormalReason:     reason,
		CompensationAction: models.CompensationNone,
		CompensationStatus: 0,
		Remarks:            fmt.Sprintf("开柜失败补偿单：原因=%s，柜机=%s，格口=%d", reason, cabinet.CabinetNo, selectedSlotNo),
	}
	if err := tx.Create(&order).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("创建异常订单失败")
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
			Reason:      fmt.Sprintf("异常租借押金冻结（待补偿）：订单%s，原因%s", orderNo, reason),
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
		tx.Model(&order).Update("deposit_status", 1)
	}

	excep := models.ExceptionRecord{
		OrderID:       order.ID,
		UserID:        userID,
		BatteryID:     batteryID,
		ExcepType:     models.ExcepUnlockFailed,
		FeeAmt:        0,
		FeeDeducted:   0,
		DepositUsed:   0,
		RefundAmt:     0,
		Status:        0,
		Description:   fmt.Sprintf("开柜失败自动创建异常单：%s，补偿动作待判定", reason),
	}
	if reason == models.AbnormalReasonCabinetOffline {
		excep.ExcepType = models.ExcepCabinetOffline
	} else if reason == models.AbnormalReasonBatteryMissing {
		excep.ExcepType = models.ExcepBatteryMissing
	} else if reason == models.AbnormalReasonLockStuck {
		excep.ExcepType = models.ExcepLockStuck
	}
	tx.Create(&excep)

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("提交事务失败")
	}

	return &ScanRentalResp{
		OrderNo:            orderNo,
		CabinetNo:          cabinet.CabinetNo,
		SlotNo:             selectedSlotNo,
		BatteryNo:          batteryNo,
		BatterySOC:         batterySOC,
		DepositAmt:         depositAmt,
		DepositFree:        depositFree,
		LockOpen:           false,
		StartTime:          time.Time{},
		Status:             string(models.OrderAbnormal),
		IsAbnormal:         true,
		AbnormalReason:     string(reason),
		CompensationAction: string(models.CompensationNone),
	}, nil
}

func (s *RentalService) createAbnormalOrderForSlot(ctx context.Context, userID uint64, cabinet *models.Cabinet, slotNo int, reason models.AbnormalReason) (*ScanRentalResp, error) {
	pSlotNo := slotNo
	return s.createAbnormalOrder(ctx, userID, cabinet, &pSlotNo, reason)
}

func (s *RentalService) markSlotAbnormal(ctx context.Context, userID uint64, cabinet *models.Cabinet, slot *models.Slot, battery *models.Battery, reason models.AbnormalReason) error {
	_ = database.DB.Model(&models.Slot{}).Where("id = ?", slot.ID).Update("status", models.SlotFault)
	return nil
}

func (s *RentalService) ReportUnlockFailure(ctx context.Context, userID uint64, req *ReportUnlockFailureReq) (*ScanRentalResp, error) {
	var order models.RentalOrder
	if err := database.DB.Where("order_no = ? AND user_id = ?", req.OrderNo, userID).First(&order).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("订单不存在")
		}
		return nil, fmt.Errorf("查询订单失败")
	}

	if order.Status != models.OrderPending && order.Status != models.OrderRenting {
		return nil, fmt.Errorf("订单状态不允许上报开柜失败")
	}

	reason := models.AbnormalReason(req.Reason)
	if reason != models.AbnormalReasonUnlockDoorNotOpen &&
		reason != models.AbnormalReasonBatteryMissing &&
		reason != models.AbnormalReasonLockStuck {
		reason = models.AbnormalReasonUnlockDoorNotOpen
	}

	lockKey := fmt.Sprintf("order_comp:%d", order.ID)
	lock, acquired, err := redisx.AcquireLock(ctx, lockKey, 10*time.Second, 3)
	if err != nil || !acquired {
		return nil, fmt.Errorf("处理中，请稍后再试")
	}
	defer lock.Release(ctx)

	tx := database.DB.Begin()

	excepType := models.ExcepUnlockFailed
	if reason == models.AbnormalReasonBatteryMissing {
		excepType = models.ExcepBatteryMissing
	} else if reason == models.AbnormalReasonLockStuck {
		excepType = models.ExcepLockStuck
	}

	excep := models.ExceptionRecord{
		OrderID:       order.ID,
		UserID:        userID,
		BatteryID:     order.BatteryID,
		ExcepType:     excepType,
		FeeAmt:        0,
		FeeDeducted:   0,
		DepositUsed:   0,
		RefundAmt:     0,
		Status:        0,
		Evidence:      req.Evidence,
		Description:   fmt.Sprintf("用户主动上报开柜失败：%s", reason),
	}
	tx.Create(&excep)

	updates := map[string]interface{}{
		"status":              models.OrderAbnormal,
		"abnormal_reason":     reason,
		"billing_enabled":     false,
		"compensation_action": models.CompensationNone,
		"compensation_status": 0,
		"start_time":          nil,
		"remarks":             fmt.Sprintf("%s | 用户上报开柜失败：%s", order.Remarks, reason),
	}
	if err := tx.Model(&order).Updates(updates).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("更新订单失败")
	}

	order.Status = models.OrderAbnormal
	order.AbnormalReason = reason
	order.BillingEnabled = false

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("提交事务失败")
	}

	var cab models.Cabinet
	var slot models.Slot
	var bat models.Battery
	cabinetNo := ""
	slotNo := 0
	batteryNo := ""
	batterySOC := 0
	if database.DB.First(&cab, order.FromCabinetID).Error == nil {
		cabinetNo = cab.CabinetNo
	}
	if database.DB.First(&slot, order.FromSlotID).Error == nil {
		slotNo = slot.SlotNo
	}
	if database.DB.First(&bat, order.BatteryID).Error == nil {
		batteryNo = bat.BatteryNo
		batterySOC = bat.SOC
	}

	return &ScanRentalResp{
		OrderNo:            order.OrderNo,
		CabinetNo:          cabinetNo,
		SlotNo:             slotNo,
		BatteryNo:          batteryNo,
		BatterySOC:         batterySOC,
		DepositAmt:         order.DepositAmt,
		DepositFree:        order.DepositAmt == 0,
		LockOpen:           false,
		StartTime:          time.Time{},
		Status:             string(models.OrderAbnormal),
		IsAbnormal:         true,
		AbnormalReason:     string(reason),
		CompensationAction: string(models.CompensationNone),
	}, nil
}
