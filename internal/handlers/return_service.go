package handlers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"battery-rental/internal/database"
	"battery-rental/internal/models"
	"battery-rental/internal/redisx"
	"battery-rental/internal/utils"

	"gorm.io/gorm"
)

type ReturnService struct{}

func NewReturnService() *ReturnService {
	return &ReturnService{}
}

type DetectBatteryReq struct {
	CabinetNo    string  `json:"cabinet_no" binding:"required"`
	SlotNo       int     `json:"slot_no" binding:"required"`
	BatteryNo    string  `json:"battery_no" binding:"required"`
	SOC          int     `json:"soc"`
	Temperature  float64 `json:"temperature"`
	DeviceTime   string  `json:"device_time"`
	ReportSeq    int64   `json:"report_seq"`
}

type DetectBatteryResp struct {
	OrderNo       string  `json:"order_no"`
	UserID        uint64  `json:"user_id"`
	Returned      bool    `json:"returned"`
	CrossCabinet  bool    `json:"cross_cabinet"`
	DurationSec   int64   `json:"duration_sec"`
	TotalFee      int64   `json:"total_fee"`
	DepositAmt    int64   `json:"deposit_amt"`
	RefundAmt     int64   `json:"refund_amt"`
	FeeCapHit     bool    `json:"fee_cap_hit"`
	ActualPay     int64   `json:"actual_pay"`
	Status        string  `json:"status"`
	Remarks       string  `json:"remarks"`
}

func (s *ReturnService) DetectBatteryReturn(ctx context.Context, req *DetectBatteryReq) (*DetectBatteryResp, error) {
	reportKey := fmt.Sprintf("report:%s:%d:%d", req.CabinetNo, req.SlotNo, req.ReportSeq)
	if req.ReportSeq > 0 {
		exist, _ := redisx.SetNXWithTTL(ctx, reportKey, "1", 7*24*time.Hour)
		if !exist {
			return nil, fmt.Errorf("重复上报")
		}
	}

	var cabinet models.Cabinet
	if err := database.DB.Where("cabinet_no = ?", req.CabinetNo).First(&cabinet).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("柜机不存在")
		}
		return nil, fmt.Errorf("查询柜机失败")
	}

	var slot models.Slot
	if err := database.DB.Where("cabinet_id = ? AND slot_no = ?", cabinet.ID, req.SlotNo).First(&slot).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("格口不存在")
		}
		return nil, fmt.Errorf("查询格口失败")
	}

	var battery models.Battery
	if err := database.DB.Where("battery_no = ?", req.BatteryNo).First(&battery).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("电池编号不存在")
		}
		return nil, fmt.Errorf("查询电池失败")
	}

	lockKey := fmt.Sprintf("return:%s", req.BatteryNo)
	lock, acquired, err := redisx.AcquireLock(ctx, lockKey, 15*time.Second, 3)
	if err != nil {
		return nil, fmt.Errorf("系统繁忙")
	}
	if !acquired {
		return nil, fmt.Errorf("归还处理中，请稍候")
	}
	defer lock.Release(ctx)

	var order models.RentalOrder
	if err := database.DB.Where("battery_id = ? AND status IN ?", battery.ID,
		[]models.OrderStatus{models.OrderRenting, models.OrderReturning}).
		Order("id DESC").First(&order).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("该电池无进行中的租借订单")
		}
		return nil, fmt.Errorf("查询订单失败")
	}

	if order.Status == models.OrderReturning {
		return &DetectBatteryResp{
			OrderNo: order.OrderNo,
			Returned: true,
			Status: string(order.Status),
			Remarks: "已处理归还",
		}, nil
	}

	crossCabinet := order.FromCabinetID != cabinet.ID

	tx := database.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var rule models.BillingRule
	if err := tx.First(&rule, order.RuleID).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("查询计费规则失败")
	}

	now := time.Now()
	startTime := order.StartTime
	if startTime == nil {
		startTime = &now
	}
	durationSec := int64(now.Sub(*startTime).Seconds())
	if durationSec < 0 {
		durationSec = 0
	}

	totalFee, capHit, _ := utils.CalcFee(
		durationSec,
		rule.FirstFreeMin, rule.FirstPeriodMin, rule.FirstPrice,
		rule.UnitMin, rule.UnitPrice, rule.DailyCap, rule.MaxDays, rule.MaxFee,
	)

	refundAmt := int64(0)
	depositAmt := order.DepositAmt
	actualPay := totalFee

	if order.DepositStatus == 1 && depositAmt > 0 {
		var user models.User
		if err := tx.Clauses(clauseUpdateLock).First(&user, order.UserID).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("查询用户失败")
		}

		if totalFee >= depositAmt {
			actualPay = totalFee
			refundAmt = 0
			feeDeductTxn := utils.GenTxnID()
			deductRec := models.DepositRecord{
				OrderID:     order.ID,
				UserID:      order.UserID,
				Action:      models.DepositDeduct,
				Amount:      depositAmt,
				BeforeBal:   user.Balance,
				AfterBal:    user.Balance,
				TxnID:       feeDeductTxn,
				Status:      1,
				Reason:      fmt.Sprintf("押金抵扣租金：订单%s，抵扣%d分", order.OrderNo, depositAmt),
				ProcessedAt: &now,
			}
			if err := tx.Create(&deductRec).Error; err != nil {
				tx.Rollback()
				return nil, fmt.Errorf("创建押金抵扣记录失败")
			}
		} else {
			refundPart := depositAmt - totalFee
			actualPay = totalFee
			refundAmt = refundPart
			user.Balance += refundPart

			feeDeductTxn := utils.GenTxnID()
			deductRec := models.DepositRecord{
				OrderID:     order.ID,
				UserID:      order.UserID,
				Action:      models.DepositDeduct,
				Amount:      totalFee,
				BeforeBal:   user.Balance - refundPart,
				AfterBal:    user.Balance,
				TxnID:       feeDeductTxn,
				Status:      1,
				Reason:      fmt.Sprintf("押金抵扣租金：订单%s，抵扣%d分", order.OrderNo, totalFee),
				ProcessedAt: &now,
			}
			if err := tx.Create(&deductRec).Error; err != nil {
				tx.Rollback()
				return nil, fmt.Errorf("创建押金抵扣记录失败")
			}

			refundTxn := utils.GenTxnID()
			refundRec := models.DepositRecord{
				OrderID:     order.ID,
				UserID:      order.UserID,
				Action:      models.DepositRelease,
				Amount:      refundPart,
				BeforeBal:   user.Balance - refundPart,
				AfterBal:    user.Balance,
				TxnID:       refundTxn,
				Status:      1,
				Reason:      fmt.Sprintf("押金释放退款：订单%s，退还%d分", order.OrderNo, refundPart),
				ProcessedAt: &now,
			}
			if err := tx.Create(&refundRec).Error; err != nil {
				tx.Rollback()
				return nil, fmt.Errorf("创建押金退款记录失败")
			}
			if err := tx.Save(&user).Error; err != nil {
				tx.Rollback()
				return nil, fmt.Errorf("更新用户余额失败")
			}
		}
	}

	returnRec := models.ReturnRecord{
		OrderID:      order.ID,
		CabinetID:    cabinet.ID,
		SlotID:       slot.ID,
		BatteryID:    battery.ID,
		UserID:       order.UserID,
		DetectTime:   now,
		LockTime:     &now,
		SOC:          req.SOC,
		Temperature:  req.Temperature,
		CrossCabinet: crossCabinet,
		FeeCalc:      totalFee,
		FeeCapHit:    capHit,
		Remarks:      fmt.Sprintf("归还检测：柜机%s格口%d", cabinet.CabinetNo, req.SlotNo),
	}
	if err := tx.Create(&returnRec).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("创建归还记录失败")
	}

	slot.Status = models.SlotOccupied
	slot.BatteryID = &battery.ID
	slot.LockStatus = 1
	slot.LastAction = &now
	if err := tx.Save(&slot).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("更新格口状态失败")
	}

	battery.Status = models.BatteryInCabinet
	battery.SlotID = &slot.ID
	battery.SOC = req.SOC
	battery.Temperature = req.Temperature
	battery.CycleCount++
	battery.LastReportAt = &now
	if err := tx.Save(&battery).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("更新电池状态失败")
	}

	endTime := now
	order.Status = models.OrderCompleted
	order.ToCabinetID = &cabinet.ID
	order.ToSlotID = &slot.ID
	order.EndTime = &endTime
	order.DurationSec = durationSec
	order.TotalFee = totalFee
	order.PaidFee = actualPay
	order.RefundAmt = refundAmt
	order.EndSOC = req.SOC
	order.CrossCabinet = crossCabinet
	order.ExceptionFee = 0
	order.DepositStatus = 2
	if err := tx.Save(&order).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("更新订单失败")
	}

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("提交事务失败")
	}

	_ = redisx.Del(ctx, fmt.Sprintf("order:slot:%d", order.FromSlotID))

	return &DetectBatteryResp{
		OrderNo:      order.OrderNo,
		UserID:       order.UserID,
		Returned:     true,
		CrossCabinet: crossCabinet,
		DurationSec:  durationSec,
		TotalFee:     totalFee,
		DepositAmt:   depositAmt,
		RefundAmt:    refundAmt,
		FeeCapHit:    capHit,
		ActualPay:    actualPay,
		Status:       string(models.OrderCompleted),
		Remarks:      s.buildReturnRemarks(capHit, crossCabinet, durationSec, totalFee),
	}, nil
}

func (s *ReturnService) buildReturnRemarks(capHit, cross bool, dur int64, fee int64) string {
	mins := dur / 60
	base := fmt.Sprintf("使用%d分钟，费用%d分", mins, fee)
	if capHit {
		base += "（已达费用封顶）"
	}
	if cross {
		base += "，跨柜归还"
	}
	return base
}

type ManualReturnReq struct {
	OrderNo      string `json:"order_no" binding:"required"`
	CabinetNo    string `json:"cabinet_no" binding:"required"`
	SlotNo       int    `json:"slot_no" binding:"required"`
	OperatorID   uint64 `json:"operator_id"`
	Reason       string `json:"reason"`
}

func (s *ReturnService) ManualReturn(ctx context.Context, req *ManualReturnReq) (*DetectBatteryResp, error) {
	var order models.RentalOrder
	if err := database.DB.Where("order_no = ?", req.OrderNo).First(&order).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("订单不存在")
		}
		return nil, fmt.Errorf("查询订单失败")
	}
	if order.Status != models.OrderRenting {
		return nil, fmt.Errorf("订单状态非租借中")
	}

	var cabinet models.Cabinet
	if err := database.DB.Where("cabinet_no = ?", req.CabinetNo).First(&cabinet).Error; err != nil {
		return nil, fmt.Errorf("柜机不存在")
	}

	var battery models.Battery
	if err := database.DB.First(&battery, order.BatteryID).Error; err != nil {
		return nil, fmt.Errorf("电池不存在")
	}

	lockKey := fmt.Sprintf("return:%s", battery.BatteryNo)
	lock, acquired, err := redisx.AcquireLock(ctx, lockKey, 15*time.Second, 3)
	if err != nil {
		return nil, fmt.Errorf("系统繁忙")
	}
	if !acquired {
		return nil, fmt.Errorf("归还处理中")
	}
	defer lock.Release(ctx)

	tx := database.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var slot models.Slot
	if err := tx.Clauses(clauseUpdateLock).Where("cabinet_id = ? AND slot_no = ?", cabinet.ID, req.SlotNo).
		First(&slot).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("格口不存在")
	}
	if slot.Status == models.SlotOccupied {
		tx.Rollback()
		return nil, fmt.Errorf("目标格口已占用")
	}

	var rule models.BillingRule
	if err := tx.First(&rule, order.RuleID).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("计费规则失败")
	}

	now := time.Now()
	startTime := order.StartTime
	if startTime == nil {
		startTime = &now
	}
	durationSec := int64(now.Sub(*startTime).Seconds())
	totalFee, capHit, _ := utils.CalcFee(
		durationSec,
		rule.FirstFreeMin, rule.FirstPeriodMin, rule.FirstPrice,
		rule.UnitMin, rule.UnitPrice, rule.DailyCap, rule.MaxDays, rule.MaxFee,
	)

	refundAmt := int64(0)
	depositAmt := order.DepositAmt
	actualPay := totalFee

	if order.DepositStatus == 1 && depositAmt > 0 {
		var user models.User
		if err := tx.Clauses(clauseUpdateLock).First(&user, order.UserID).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("查询用户失败")
		}
		if totalFee >= depositAmt {
			actualPay = totalFee
			txnID := utils.GenTxnID()
			_ = tx.Create(&models.DepositRecord{
				OrderID: order.ID, UserID: order.UserID,
				Action: models.DepositDeduct, Amount: depositAmt,
				BeforeBal: user.Balance, AfterBal: user.Balance,
				TxnID: txnID, Status: 1,
				Reason: fmt.Sprintf("押金抵扣租金：%s（运营人工归还）", order.OrderNo),
				ProcessedAt: &now,
			})
		} else {
			refundAmt = depositAmt - totalFee
			user.Balance += refundAmt
			txnID1 := utils.GenTxnID()
			_ = tx.Create(&models.DepositRecord{
				OrderID: order.ID, UserID: order.UserID,
				Action: models.DepositDeduct, Amount: totalFee,
				BeforeBal: user.Balance - refundAmt, AfterBal: user.Balance,
				TxnID: txnID1, Status: 1,
				Reason: fmt.Sprintf("押金抵扣租金：%s（人工）", order.OrderNo), ProcessedAt: &now,
			})
			txnID2 := utils.GenTxnID()
			_ = tx.Create(&models.DepositRecord{
				OrderID: order.ID, UserID: order.UserID,
				Action: models.DepositRelease, Amount: refundAmt,
				BeforeBal: user.Balance - refundAmt, AfterBal: user.Balance,
				TxnID: txnID2, Status: 1,
				Reason: fmt.Sprintf("押金释放退款：%s（人工）", order.OrderNo), ProcessedAt: &now,
			})
			_ = tx.Save(&user)
		}
	}

	crossCabinet := order.FromCabinetID != cabinet.ID

	_ = tx.Create(&models.ReturnRecord{
		OrderID: order.ID, CabinetID: cabinet.ID, SlotID: slot.ID,
		BatteryID: battery.ID, UserID: order.UserID,
		DetectTime: now, LockTime: &now, SOC: battery.SOC,
		CrossCabinet: crossCabinet, FeeCalc: totalFee, FeeCapHit: capHit,
		Remarks: fmt.Sprintf("运营人工归还：%s，原因：%s", req.Reason, req.Reason),
	})

	slot.Status = models.SlotOccupied
	slot.BatteryID = &battery.ID
	slot.LastAction = &now
	_ = tx.Save(&slot)

	battery.Status = models.BatteryInCabinet
	battery.SlotID = &slot.ID
	battery.LastReportAt = &now
	_ = tx.Save(&battery)

	order.Status = models.OrderCompleted
	order.ToCabinetID = &cabinet.ID
	order.ToSlotID = &slot.ID
	order.EndTime = &now
	order.DurationSec = durationSec
	order.TotalFee = totalFee
	order.PaidFee = actualPay
	order.RefundAmt = refundAmt
	order.CrossCabinet = crossCabinet
	order.DepositStatus = 2
	order.Remarks += fmt.Sprintf(" | 人工归还:%s", req.Reason)
	_ = tx.Save(&order)

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("提交事务失败")
	}

	_ = redisx.Del(ctx, fmt.Sprintf("order:slot:%d", order.FromSlotID))

	return &DetectBatteryResp{
		OrderNo: order.OrderNo, UserID: order.UserID,
		Returned: true, CrossCabinet: crossCabinet,
		DurationSec: durationSec, TotalFee: totalFee,
		DepositAmt: depositAmt, RefundAmt: refundAmt,
		FeeCapHit: capHit, ActualPay: actualPay,
		Status: string(models.OrderCompleted),
		Remarks: "人工归还完成",
	}, nil
}

func (s *ReturnService) GetActiveOrder(ctx context.Context, userID uint64) (*models.RentalOrder, error) {
	var order models.RentalOrder
	err := database.DB.Where("user_id = ? AND status IN ?", userID,
		[]models.OrderStatus{models.OrderPending, models.OrderRenting, models.OrderReturning}).
		Preload("Battery").Preload("Rule").
		Order("id DESC").First(&order).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &order, nil
}

func (s *ReturnService) ListOrders(ctx context.Context, userID uint64, page, size int) ([]models.RentalOrder, int64, error) {
	var list []models.RentalOrder
	var total int64
	q := database.DB.Model(&models.RentalOrder{}).Where("user_id = ?", userID)
	q.Count(&total)
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	err := q.Preload("Battery").Preload("Rule").
		Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&list).Error
	return list, total, err
}
