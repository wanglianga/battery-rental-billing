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
	CrossCategory string  `json:"cross_category,omitempty"`
	DurationSec   int64   `json:"duration_sec"`
	TotalFee      int64   `json:"total_fee"`
	DepositAmt    int64   `json:"deposit_amt"`
	RefundAmt     int64   `json:"refund_amt"`
	FeeCapHit     bool    `json:"fee_cap_hit"`
	ActualPay     int64   `json:"actual_pay"`
	Status        string  `json:"status"`
	Remarks       string  `json:"remarks"`
	NeedManualReview bool `json:"need_manual_review,omitempty"`
	JudgeSummary  string  `json:"judge_summary,omitempty"`
	JudgeDetail   *CrossReturnJudge `json:"judge_detail,omitempty"`
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
	var orderFound bool
	orderErr := database.DB.Where("battery_id = ? AND status IN ?", battery.ID,
		[]models.OrderStatus{models.OrderRenting, models.OrderReturning}).
		Order("id DESC").First(&order).Error
	if orderErr == nil {
		orderFound = true
	} else if !errors.Is(orderErr, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("查询订单失败")
	}

	if orderFound && order.Status == models.OrderReturning {
		return &DetectBatteryResp{
			OrderNo:  order.OrderNo,
			Returned: true,
			Status:   string(order.Status),
			Remarks:  "已处理归还",
		}, nil
	}

	crossCabinet := false
	if orderFound {
		crossCabinet = order.FromCabinetID != cabinet.ID
	}

	judges := s.evaluateCrossReturn(req, &cabinet, &slot, &battery, orderFound, &order, crossCabinet)
	category, catSummary := s.classifyCrossReturn(judges, orderFound, &battery)
	judgeSummary := s.buildJudgeSummary(judges, category)
	needManual := (category == models.CrossReturnSuspectSwapped) || (category == models.CrossReturnUnclaimed)

	tx := database.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	now := time.Now()
	durationSec := int64(0)
	totalFee := int64(0)
	capHit := false
	refundAmt := int64(0)
	depositAmt := int64(0)
	actualPay := int64(0)
	orderUserID := uint64(0)
	orderNo := ""
	respStatus := string(models.OrderCompleted)

	if orderFound {
		var rule models.BillingRule
		if err := tx.First(&rule, order.RuleID).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("查询计费规则失败")
		}

		startTime := order.StartTime
		if startTime == nil {
			startTime = &now
		}
		durationSec = int64(now.Sub(*startTime).Seconds())
		if durationSec < 0 {
			durationSec = 0
		}

		totalFee, capHit, _ = utils.CalcFee(
			durationSec,
			rule.FirstFreeMin, rule.FirstPeriodMin, rule.FirstPrice,
			rule.UnitMin, rule.UnitPrice, rule.DailyCap, rule.MaxDays, rule.MaxFee,
		)

		refundAmt = int64(0)
		depositAmt = order.DepositAmt
		actualPay = totalFee
		orderUserID = order.UserID
		orderNo = order.OrderNo

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
		order.Remarks += fmt.Sprintf(" | 跨柜归还分类=%s", category)
		if needManual {
			order.Status = models.OrderDisputed
			order.Remarks += fmt.Sprintf(" | %s", catSummary)
			respStatus = string(models.OrderDisputed)
		}
		if err := tx.Save(&order).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("更新订单失败")
		}
	} else {
		respStatus = string(category)
		if category == models.CrossReturnSuspectSwapped {
			orderUserID = 0
		}
	}

	returnRecOrderID := uint64(0)
	if orderFound {
		returnRecOrderID = order.ID
	}
	returnRec := models.ReturnRecord{
		OrderID:             returnRecOrderID,
		CabinetID:           cabinet.ID,
		SlotID:              slot.ID,
		BatteryID:           battery.ID,
		UserID:              orderUserID,
		DetectTime:          now,
		LockTime:            &now,
		SOC:                 req.SOC,
		Temperature:         req.Temperature,
		CrossCabinet:        crossCabinet,
		CrossCategory:       category,
		JBatteryNoMatch:     judges.BatteryNoMatch,
		JBatteryNoEvidence:  judges.BatteryNoEvidence,
		JOriginalOrderValid: judges.OriginalOrderValid,
		JOriginalOrderEvidence: judges.OriginalOrderEvidence,
		JSlotStateOK:        judges.SlotStateOK,
		JSlotStateEvidence:  judges.SlotStateEvidence,
		JSOCOK:              judges.SOCOK,
		JSOCEvidence:        judges.SOCEvidence,
		JTemperatureOK:      judges.TemperatureOK,
		JTemperatureEvidence: judges.TemperatureEvidence,
		JFromCabinetMatch:    judges.FromCabinetMatch,
		JFromCabinetEvidence: judges.FromCabinetEvidence,
		JUserIdentityMatch:   judges.UserIdentityMatch,
		JUserIdentityEvidence: judges.UserIdentityEvidence,
		JBatteryStatusHistory: judges.BatteryStatusHistory,
		JudgeSummary:        judgeSummary,
		FeeCalc:             totalFee,
		FeeCapHit:           capHit,
		Remarks:             fmt.Sprintf("归还检测：柜机%s格口%d，分类=%s，%s", cabinet.CabinetNo, req.SlotNo, category, catSummary),
	}
	if err := tx.Create(&returnRec).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("创建归还记录失败")
	}

	if !orderFound {
		excepType := models.ExcepUnclaimedBattery
		excepDesc := catSummary
		if category == models.CrossReturnRepairInbound {
			excepType = models.ExcepRepairInbound
		} else if category == models.CrossReturnSuspectSwapped {
			excepType = models.ExcepCrossSuspicion
		}
		tx.Create(&models.ExceptionRecord{
			OrderID:     0,
			UserID:      0,
			BatteryID:   battery.ID,
			ExcepType:   excepType,
			FeeAmt:      0,
			FeeDeducted: 0,
			DepositUsed: 0,
			RefundAmt:   0,
			Status:      0,
			Evidence:    judgeSummary,
			Description: excepDesc,
		})
	} else if needManual {
		excepType := models.ExcepCrossSuspicion
		if category == models.CrossReturnSuspectSwapped {
			excepType = models.ExcepCrossSuspicion
		}
		tx.Create(&models.ExceptionRecord{
			OrderID:     order.ID,
			UserID:      order.UserID,
			BatteryID:   battery.ID,
			ExcepType:   excepType,
			FeeAmt:      0,
			FeeDeducted: 0,
			DepositUsed: 0,
			RefundAmt:   0,
			Status:      0,
			Evidence:    judgeSummary,
			Description: catSummary,
		})
	}

	slot.Status = models.SlotOccupied
	slot.BatteryID = &battery.ID
	slot.LockStatus = 1
	slot.LastAction = &now
	if err := tx.Save(&slot).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("更新格口状态失败")
	}

	newBatStatus := models.BatteryInCabinet
	if category == models.CrossReturnRepairInbound {
		newBatStatus = models.BatteryRepair
	}
	battery.Status = newBatStatus
	battery.SlotID = &slot.ID
	battery.SOC = req.SOC
	battery.Temperature = req.Temperature
	battery.CycleCount++
	battery.LastReportAt = &now
	if err := tx.Save(&battery).Error; err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("更新电池状态失败")
	}

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("提交事务失败")
	}

	if orderFound {
		_ = redisx.Del(ctx, fmt.Sprintf("order:slot:%d", order.FromSlotID))
	}

	return &DetectBatteryResp{
		OrderNo:          orderNo,
		UserID:           orderUserID,
		Returned:         true,
		CrossCabinet:     crossCabinet,
		CrossCategory:    string(category),
		DurationSec:      durationSec,
		TotalFee:         totalFee,
		DepositAmt:       depositAmt,
		RefundAmt:        refundAmt,
		FeeCapHit:        capHit,
		ActualPay:        actualPay,
		Status:           respStatus,
		Remarks:          s.buildReturnRemarks(capHit, crossCabinet, durationSec, totalFee) + " | " + catSummary,
		NeedManualReview: needManual,
		JudgeSummary:     judgeSummary,
		JudgeDetail:      judges,
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

type CrossReturnJudge struct {
	BatteryNoMatch         bool
	BatteryNoEvidence      string
	OriginalOrderValid     bool
	OriginalOrderEvidence  string
	SlotStateOK            bool
	SlotStateEvidence      string
	SOCOK                  bool
	SOCEvidence            string
	TemperatureOK          bool
	TemperatureEvidence    string
	FromCabinetMatch       bool
	FromCabinetEvidence    string
	UserIdentityMatch      bool
	UserIdentityEvidence   string
	BatteryStatusHistory   string
}

func (s *ReturnService) evaluateCrossReturn(req *DetectBatteryReq, cabinet *models.Cabinet, slot *models.Slot, battery *models.Battery, orderFound bool, order *models.RentalOrder, crossCabinet bool) *CrossReturnJudge {
	j := &CrossReturnJudge{}

	j.BatteryNoMatch = req.BatteryNo == battery.BatteryNo
	if j.BatteryNoMatch {
		j.BatteryNoEvidence = fmt.Sprintf("[OK] 上报电池编号[%s]与系统登记电池[%s]完全一致", req.BatteryNo, battery.BatteryNo)
	} else {
		j.BatteryNoEvidence = fmt.Sprintf("[WARN] 上报电池编号[%s]与系统查询电池[%s]存在字符级差异（需确认是否上报笔误）", req.BatteryNo, battery.BatteryNo)
	}

	var fromCab *models.Cabinet
	if orderFound && order.ID > 0 && order.FromCabinetID > 0 {
		var fc models.Cabinet
		if database.DB.First(&fc, order.FromCabinetID).Error == nil {
			fromCab = &fc
		}
	}
	j.FromCabinetMatch = !crossCabinet
	if crossCabinet {
		fromInfo := ""
		if fromCab != nil {
			fromInfo = fmt.Sprintf("%s(%s)", fromCab.CabinetNo, fromCab.Name)
		} else {
			fromInfo = fmt.Sprintf("ID=%d", order.FromCabinetID)
		}
		j.FromCabinetEvidence = fmt.Sprintf("[CROSS] 跨网点归还：从[%s] → 归还到[%s(%s)]，系统已记录跨柜标记",
			fromInfo, cabinet.CabinetNo, cabinet.Name)
	} else {
		j.FromCabinetEvidence = fmt.Sprintf("[NORMAL] 同网点归还：起始与归还均为[%s(%s)]，无跨柜情况", cabinet.CabinetNo, cabinet.Name)
	}

	if orderFound && order.ID > 0 {
		var orderBat models.Battery
		batOK := database.DB.First(&orderBat, order.BatteryID).Error == nil
		var orderUser models.User
		userOK := database.DB.First(&orderUser, order.UserID).Error == nil

		if batOK && orderBat.BatteryNo == battery.BatteryNo {
			j.OriginalOrderValid = true
			ordStatus := string(order.Status)
			j.OriginalOrderEvidence = fmt.Sprintf("[OK] 原订单校验通过：订单号[%s]，创建=%s，状态=%s，订单电池=%s，归还电池=%s，两者完全匹配",
				order.OrderNo, order.CreatedAt.Format("2006-01-02 15:04:05"), ordStatus, orderBat.BatteryNo, battery.BatteryNo)
			if userOK {
				j.UserIdentityMatch = true
				j.UserIdentityEvidence = fmt.Sprintf("[OK] 订单用户一致：ID=%d，手机号=%s，角色=%s",
					orderUser.ID, orderUser.Phone, orderUser.Role)
			} else {
				j.UserIdentityMatch = false
				j.UserIdentityEvidence = fmt.Sprintf("[WARN] 订单用户信息查询失败，但不影响本次归还结算判定")
			}
		} else if batOK {
			j.OriginalOrderValid = false
			var batLastRental models.RentalOrder
			recentOK := database.DB.Where("battery_id = ?", battery.ID).Order("id DESC").First(&batLastRental).Error == nil
			recentInfo := ""
			if recentOK {
				recentInfo = fmt.Sprintf("；该电池最近订单=%s(状态=%s)", batLastRental.OrderNo, batLastRental.Status)
			}
			j.OriginalOrderEvidence = fmt.Sprintf("[SUSPECT] 疑似串电池：当前进行中订单[%s]关联电池是[%s]，但实际归还的是[%s]，两者不匹配%s",
				order.OrderNo, orderBat.BatteryNo, battery.BatteryNo, recentInfo)
			j.UserIdentityMatch = false
			j.UserIdentityEvidence = fmt.Sprintf("[SUSPECT] 因电池不匹配，用户身份关联存疑，建议人工核对租借凭证")
		} else {
			j.OriginalOrderValid = true
			j.OriginalOrderEvidence = fmt.Sprintf("[OK] 找到进行中订单[%s]，订单电池详情查询失败但继续处理（DB查询异常降级）", order.OrderNo)
		}
	} else {
		j.OriginalOrderValid = false
		var historyCnt int64
		var lastRental models.RentalOrder
		database.DB.Model(&models.RentalOrder{}).Where("battery_id = ?", battery.ID).Count(&historyCnt)
		hasRecent := database.DB.Where("battery_id = ?", battery.ID).Order("id DESC").First(&lastRental).Error == nil
		historyInfo := ""
		if historyCnt > 0 {
			if hasRecent {
				historyInfo = fmt.Sprintf("，最近一次订单=%s(状态=%s，创建=%s)",
					lastRental.OrderNo, lastRental.Status, lastRental.CreatedAt.Format("2006-01-02 15:04:05"))
			}
			j.OriginalOrderEvidence = fmt.Sprintf("[UNCLAIMED] 未找到该电池的进行中订单，但历史租借记录=%d条%s，标记为无人认领待核验",
				historyCnt, historyInfo)
		} else {
			j.OriginalOrderEvidence = fmt.Sprintf("[REPAIR?] 电池[%s]无任何租借历史记录，可能是新电池维修入库、库存调拨或串电池混入，请运营确认来源",
				battery.BatteryNo)
		}
		j.UserIdentityMatch = false
		j.UserIdentityEvidence = fmt.Sprintf("[NONE] 无进行中订单关联用户，电池当前状态=%s，循环次数=%d",
			battery.Status, battery.CycleCount)
	}

	j.SlotStateOK = slot.Status != models.SlotFault && slot.Status != models.SlotOccupied
	slotBatteryInfo := "空"
	if slot.BatteryID != nil {
		var slotBat models.Battery
		if database.DB.First(&slotBat, *slot.BatteryID).Error == nil {
			slotBatteryInfo = slotBat.BatteryNo
		} else {
			slotBatteryInfo = fmt.Sprintf("ID=%d", *slot.BatteryID)
		}
	}
	if j.SlotStateOK {
		j.SlotStateEvidence = fmt.Sprintf("[OK] 目标格口校验通过：格口#%d，状态=%s，锁状态=%d(1=正常)，当前绑定电池=%s，可安全入库",
			slot.SlotNo, slot.Status, slot.LockStatus, slotBatteryInfo)
	} else {
		reason := ""
		if slot.Status == models.SlotFault {
			reason = "格口处于故障状态"
		} else if slot.Status == models.SlotOccupied {
			reason = fmt.Sprintf("格口已被电池[%s]占用", slotBatteryInfo)
		} else {
			reason = fmt.Sprintf("状态=%s异常", slot.Status)
		}
		j.SlotStateEvidence = fmt.Sprintf("[WARN] 目标格口异常：格口#%d，%s，锁状态=%d，请运营检查硬件状态",
			slot.SlotNo, reason, slot.LockStatus)
	}

	j.SOCOK = req.SOC >= 10 && req.SOC <= 100
	socDiff := 0
	if orderFound && order.StartSOC > 0 {
		socDiff = req.SOC - order.StartSOC
	}
	socDiffStr := ""
	if orderFound {
		socDiffStr = fmt.Sprintf("，租借时=%d%%，差值=%+d%%", order.StartSOC, socDiff)
	}
	if j.SOCOK {
		level := "充足"
		if req.SOC < 30 {
			level = "偏低（建议尽快充电）"
		} else if req.SOC >= 80 {
			level = "高"
		}
		j.SOCEvidence = fmt.Sprintf("[OK] 电量校验通过：SOC=%d%%，等级=%s，正常范围=[10,100]%s",
			req.SOC, level, socDiffStr)
	} else if req.SOC < 10 {
		j.SOCEvidence = fmt.Sprintf("[DAMAGE?] 电量异常偏低：SOC=%d%%<10%%阈值%s，可能存在电池损坏、漏液或BMS故障",
			req.SOC, socDiffStr)
	} else {
		j.SOCEvidence = fmt.Sprintf("[ERROR] 电量上报越界：SOC=%d%%>100%%，疑似柜机传感器或上报协议异常%s",
			req.SOC, socDiffStr)
	}

	j.TemperatureOK = req.Temperature >= -10 && req.Temperature <= 55
	riskLevel := "正常"
	if !j.TemperatureOK {
		if req.Temperature < -10 {
			riskLevel = "低温风险（可能影响容量）"
		} else if req.Temperature > 55 {
			riskLevel = "过热风险（可能存在短路或外部加热）"
		}
	}
	tempHistory := ""
	if battery.LastReportAt != nil {
		tempHistory = fmt.Sprintf("，电池上次上报温度=%.1f℃(@%s)",
			battery.Temperature, battery.LastReportAt.Format("2006-01-02 15:04"))
	}
	if j.TemperatureOK {
		j.TemperatureEvidence = fmt.Sprintf("[OK] 温度校验通过：%.1f℃，范围=[-10,55]，风险等级=%s%s",
			req.Temperature, riskLevel, tempHistory)
	} else {
		j.TemperatureEvidence = fmt.Sprintf("[WARN] 温度异常：%.1f℃，超出正常范围=[-10,55]，风险等级=%s%s，运营需关注",
			req.Temperature, riskLevel, tempHistory)
	}

	batStatusDesc := map[models.BatteryStatus]string{
		models.BatteryInCabinet: "在柜",
		models.BatteryInUse:     "租借中",
		models.BatteryCharging:  "充电中",
		models.BatteryLost:      "已丢失",
		models.BatteryDamaged:   "已损坏",
		models.BatteryRepair:    "维修中",
	}
	desc, ok := batStatusDesc[battery.Status]
	if !ok {
		desc = string(battery.Status)
	}
	j.BatteryStatusHistory = fmt.Sprintf(
		"电池档案：型号=%s，容量=%dmAh，循环=%d次，当前状态=%s(%s)，入库时间=%s，最后上报=%s",
		battery.Model, battery.Capacity, battery.CycleCount,
		battery.Status, desc,
		battery.CreatedAt.Format("2006-01-02"),
		utils.OrDefaultTime(battery.LastReportAt, "未上报").Format("2006-01-02 15:04"),
	)

	return j
}

func (s *ReturnService) classifyCrossReturn(j *CrossReturnJudge, orderFound bool, battery *models.Battery) (models.CrossReturnCategory, string) {
	summary := ""

	if !orderFound {
		if battery.Status == models.BatteryRepair || battery.Status == models.BatteryDamaged {
			summary = fmt.Sprintf("电池[%s]当前状态=%s，无进行中订单，判定为维修入库；%s；%s",
				battery.BatteryNo, battery.Status, j.OriginalOrderEvidence, j.BatteryStatusHistory)
			return models.CrossReturnRepairInbound, summary
		}
		if !j.OriginalOrderValid && !j.SOCOK {
			summary = fmt.Sprintf("无进行中订单：%s；电量异常：%s；用户身份：%s；判定为无人认领电池",
				j.OriginalOrderEvidence, j.SOCEvidence, j.UserIdentityEvidence)
			return models.CrossReturnUnclaimed, summary
		}
		if !j.OriginalOrderValid {
			summary = fmt.Sprintf("无进行中订单：%s；用户身份：%s；判定为无人认领电池（运营需确认来源）",
				j.OriginalOrderEvidence, j.UserIdentityEvidence)
			return models.CrossReturnUnclaimed, summary
		}
	}

	if orderFound && !j.OriginalOrderValid {
		summary = fmt.Sprintf("进行中订单校验不通过：%s；用户身份：%s；疑似串电池/换电池",
			j.OriginalOrderEvidence, j.UserIdentityEvidence)
		return models.CrossReturnSuspectSwapped, summary
	}

	if orderFound && !j.UserIdentityMatch {
		summary = fmt.Sprintf("订单匹配但用户身份存疑：%s；%s；疑似串电池/换电池，需人工核验",
			j.UserIdentityEvidence, j.OriginalOrderEvidence)
		return models.CrossReturnSuspectSwapped, summary
	}

	if orderFound && !j.SOCOK {
		summary = fmt.Sprintf("订单匹配成功：%s；但电量异常：%s；用户身份：%s；建议人工复核",
			j.OriginalOrderEvidence, j.SOCEvidence, j.UserIdentityEvidence)
		return models.CrossReturnSuspectSwapped, summary
	}

	if orderFound && !j.TemperatureOK {
		summary = fmt.Sprintf("订单匹配成功：%s；但温度异常：%s；网点归属：%s；按正常结算，建议运营抽查",
			j.OriginalOrderEvidence, j.TemperatureEvidence, j.FromCabinetEvidence)
		return models.CrossReturnNormal, summary
	}

	if orderFound {
		summary = fmt.Sprintf("订单匹配成功：%s；网点归属：%s；用户身份：%s；跨柜校验通过：%s|%s|%s；按正常结算",
			j.OriginalOrderEvidence, j.FromCabinetEvidence, j.UserIdentityEvidence,
			j.SlotStateEvidence, j.SOCEvidence, j.TemperatureEvidence)
		return models.CrossReturnNormal, summary
	}

	summary = fmt.Sprintf("无法判定，默认转人工核验；%s；%s", j.OriginalOrderEvidence, j.BatteryStatusHistory)
	return models.CrossReturnUnclaimed, summary
}

func (s *ReturnService) buildJudgeSummary(j *CrossReturnJudge, category models.CrossReturnCategory) string {
	return fmt.Sprintf("[分类=%s] 电池编号校验：%s；原订单校验：%s；格口状态校验：%s；电量校验：%s；温度校验：%s；网点归属校验：%s；用户身份校验：%s；电池档案：%s",
		category, j.BatteryNoEvidence, j.OriginalOrderEvidence, j.SlotStateEvidence, j.SOCEvidence, j.TemperatureEvidence,
		j.FromCabinetEvidence, j.UserIdentityEvidence, j.BatteryStatusHistory)
}
