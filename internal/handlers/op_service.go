package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"battery-rental/internal/database"
	"battery-rental/internal/models"
	"battery-rental/internal/redisx"
	"battery-rental/internal/utils"

	"gorm.io/gorm"
)

type PaymentService struct{}

func NewPaymentService() *PaymentService {
	return &PaymentService{}
}

type RechargeReq struct {
	Amount   int64  `json:"amount" binding:"required,min=1"`
	PayType  string `json:"pay_type" binding:"required"`
	PayChannel string `json:"pay_channel"`
}

type RechargeResp struct {
	PayNo   string `json:"pay_no"`
	Amount  int64  `json:"amount"`
	PayType string `json:"pay_type"`
	PayURL  string `json:"pay_url"`
	Status  string `json:"status"`
}

func (s *PaymentService) Recharge(ctx context.Context, userID uint64, req *RechargeReq) (*RechargeResp, error) {
	lock, acquired, err := redisx.AcquireLock(ctx, fmt.Sprintf("pay_recharge:%d", userID), 5*time.Second, 2)
	if err != nil {
		return nil, fmt.Errorf("系统繁忙")
	}
	if !acquired {
		return nil, fmt.Errorf("操作频繁")
	}
	defer lock.Release(ctx)

	payNo := utils.GenPayNo()
	expireAt := time.Now().Add(30 * time.Minute)

	pay := models.PaymentRecord{
		PayNo:    payNo,
		OrderID:  nil,
		UserID:   userID,
		PayType:  req.PayType,
		Amount:   req.Amount,
		Status:   models.PayPending,
		Subject:  fmt.Sprintf("账户充值%d分", req.Amount),
		ExpireAt: &expireAt,
	}
	if err := database.DB.Create(&pay).Error; err != nil {
		return nil, fmt.Errorf("创建支付单失败")
	}

	mockURL := fmt.Sprintf("/mock/pay?pay_no=%s&amount=%d", payNo, req.Amount)
	return &RechargeResp{
		PayNo:   payNo,
		Amount:  req.Amount,
		PayType: req.PayType,
		PayURL:  mockURL,
		Status:  string(models.PayPending),
	}, nil
}

type PayCallbackReq struct {
	PayNo       string `json:"pay_no" binding:"required"`
	ThirdTxnNo  string `json:"third_txn_no"`
	Status      string `json:"status" binding:"required"`
	Amount      int64  `json:"amount"`
	RawCallback string `json:"raw_callback"`
	Sign        string `json:"sign"`
	Timestamp   int64  `json:"timestamp"`
}

type PayCallbackResp struct {
	Processed bool   `json:"processed"`
	Replayed  bool   `json:"replayed"`
	Status    string `json:"status"`
	Message   string `json:"message"`
}

func (s *PaymentService) HandleCallback(ctx context.Context, req *PayCallbackReq) (*PayCallbackResp, error) {
	dedupKey := fmt.Sprintf("pay_cb:%s:%s:%d", req.PayNo, req.ThirdTxnNo, req.Amount)
	ok, err := redisx.SetNXWithTTL(ctx, dedupKey, "1", 30*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("系统繁忙")
	}
	if !ok {
		return &PayCallbackResp{
			Processed: false,
			Replayed:  true,
			Status:    "duplicate",
			Message:   "重复回调，已忽略",
		}, nil
	}

	lock, acquired, err := redisx.AcquireLock(ctx, fmt.Sprintf("pay_cb_lock:%s", req.PayNo), 15*time.Second, 3)
	if err != nil {
		_ = redisx.Del(ctx, dedupKey)
		return nil, fmt.Errorf("系统繁忙")
	}
	if !acquired {
		_ = redisx.Del(ctx, dedupKey)
		return nil, fmt.Errorf("处理中，请稍后")
	}
	defer lock.Release(ctx)

	var pay models.PaymentRecord
	if err := database.DB.Where("pay_no = ?", req.PayNo).First(&pay).Error; err != nil {
		return &PayCallbackResp{
			Processed: false,
			Replayed:  false,
			Status:    "not_found",
			Message:   "支付单不存在",
		}, nil
	}

	now := time.Now()
	callbackCnt := pay.CallbackCnt + 1
	updates := map[string]interface{}{
		"callback_cnt":  callbackCnt,
		"last_callback": &now,
	}
	if req.RawCallback != "" {
		rawBytes, _ := json.Marshal(req)
		updates["raw_callback"] = string(rawBytes)
	}

	if pay.Status == models.PaySuccess || pay.Status == models.PayRefunded {
		_ = database.DB.Model(&pay).Updates(updates)
		return &PayCallbackResp{
			Processed: false,
			Replayed:  true,
			Status:    string(pay.Status),
			Message:   "订单已处理",
		}, nil
	}

	if req.Amount > 0 && pay.Amount != req.Amount {
		updates["status"] = models.PayFailed
		_ = database.DB.Model(&pay).Updates(updates)
		return &PayCallbackResp{
			Processed: false,
			Replayed:  false,
			Status:    string(models.PayFailed),
			Message:   "金额不匹配",
		}, nil
	}

	if req.Status == string(models.PaySuccess) || req.Status == "success" || req.Status == "SUCCESS" {
		tx := database.DB.Begin()
		defer func() {
			if r := recover(); r != nil {
				tx.Rollback()
			}
		}()

		var user models.User
		if err := tx.Clauses(clauseUpdateLock).First(&user, pay.UserID).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("查询用户失败")
		}

		beforeBal := user.Balance
		user.Balance += pay.Amount

		if err := tx.Save(&user).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("更新余额失败")
		}

		updates["status"] = models.PaySuccess
		updates["third_txn_no"] = &req.ThirdTxnNo
		updates["paid_at"] = &now

		if err := tx.Model(&pay).Updates(updates).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("更新支付单失败")
		}

		if pay.OrderID != nil && *pay.OrderID > 0 {
			var order models.RentalOrder
			if err := tx.Clauses(clauseUpdateLock).First(&order, *pay.OrderID).Error; err == nil {
				order.PaidFee += pay.Amount
				order.PayTxnID = &req.ThirdTxnNo
				tx.Save(&order)
			}
		}

		_ = beforeBal

		if err := tx.Commit().Error; err != nil {
			return nil, fmt.Errorf("提交失败")
		}

		return &PayCallbackResp{
			Processed: true,
			Replayed:  false,
			Status:    string(models.PaySuccess),
			Message:   fmt.Sprintf("充值成功%d分", pay.Amount),
		}, nil
	}

	updates["status"] = models.PayFailed
	_ = database.DB.Model(&pay).Updates(updates)
	return &PayCallbackResp{
		Processed: false,
		Replayed:  false,
		Status:    string(models.PayFailed),
		Message:   "支付失败：" + req.Status,
	}, nil
}

func (s *PaymentService) MockPay(ctx context.Context, payNo string) (*PayCallbackResp, error) {
	var pay models.PaymentRecord
	if err := database.DB.Where("pay_no = ?", payNo).First(&pay).Error; err != nil {
		return nil, fmt.Errorf("支付单不存在")
	}
	return s.HandleCallback(ctx, &PayCallbackReq{
		PayNo:      payNo,
		ThirdTxnNo: "MOCK-" + utils.GenTxnID(),
		Status:     string(models.PaySuccess),
		Amount:     pay.Amount,
		Timestamp:  time.Now().Unix(),
	})
}

type OpService struct{}

func NewOpService() *OpService {
	return &OpService{}
}

type HandleLostReq struct {
	OrderNo   string `json:"order_no" binding:"required"`
	Operator  uint64 `json:"operator"`
	Evidence  string `json:"evidence"`
	Desc      string `json:"description"`
	FeeCustom int64  `json:"fee_custom"`
}

func (s *OpService) MarkLost(ctx context.Context, req *HandleLostReq) error {
	lockKey := fmt.Sprintf("op_order:%s", req.OrderNo)
	lock, acquired, err := redisx.AcquireLock(ctx, lockKey, 15*time.Second, 3)
	if err != nil {
		return fmt.Errorf("系统繁忙")
	}
	if !acquired {
		return fmt.Errorf("处理中")
	}
	defer lock.Release(ctx)

	var order models.RentalOrder
	if err := database.DB.Where("order_no = ?", req.OrderNo).First(&order).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("订单不存在")
		}
		return fmt.Errorf("查询订单失败")
	}
	if order.Status != models.OrderRenting && order.Status != models.OrderDisputed {
		return fmt.Errorf("当前订单状态不允许标记丢失")
	}

	tx := database.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var rule models.BillingRule
	if err := tx.First(&rule, order.RuleID).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("查询计费规则失败")
	}

	now := time.Now()
	startTime := order.StartTime
	if startTime == nil {
		startTime = &now
	}
	dur := int64(now.Sub(*startTime).Seconds())
	baseFee, capHit, _ := utils.CalcFee(
		dur,
		rule.FirstFreeMin, rule.FirstPeriodMin, rule.FirstPrice,
		rule.UnitMin, rule.UnitPrice, rule.DailyCap, rule.MaxDays, rule.MaxFee,
	)

	lostFee := rule.LostFee
	if req.FeeCustom > 0 {
		lostFee = req.FeeCustom
	}
	totalFee := baseFee
	if totalFee < lostFee {
		totalFee = lostFee
	}

	depositUsed := int64(0)
	refundAmt := int64(0)
	if order.DepositStatus == 1 && order.DepositAmt > 0 {
		depositUsed = order.DepositAmt
		if depositUsed > totalFee {
			refundAmt = depositUsed - totalFee
		}
	}
	feeDeducted := totalFee
	if depositUsed > totalFee {
		feeDeducted = totalFee
	}

	excep := models.ExceptionRecord{
		OrderID:     order.ID,
		UserID:      order.UserID,
		BatteryID:   order.BatteryID,
		ExcepType:   models.ExcepLost,
		FeeAmt:      lostFee,
		FeeDeducted: feeDeducted,
		DepositUsed: depositUsed,
		RefundAmt:   refundAmt,
		Status:      1,
		HandledBy:   &req.Operator,
		HandledAt:   &now,
		Evidence:    req.Evidence,
		Description: req.Desc,
	}
	if err := tx.Create(&excep).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("创建异常记录失败")
	}

	if order.DepositStatus == 1 && order.DepositAmt > 0 {
		var user models.User
		if err := tx.Clauses(clauseUpdateLock).First(&user, order.UserID).Error; err == nil {
			if refundAmt > 0 {
				user.Balance += refundAmt
				tx.Save(&user)
				tx.Create(&models.DepositRecord{
					OrderID:     order.ID,
					UserID:      order.UserID,
					Action:      models.DepositRelease,
					Amount:      refundAmt,
					BeforeBal:   user.Balance - refundAmt,
					AfterBal:    user.Balance,
					TxnID:       utils.GenTxnID(),
					Status:      1,
					Reason:      fmt.Sprintf("丢失处理押金余额释放：%s，扣除费用%d，退还%d", order.OrderNo, totalFee, refundAmt),
					ProcessedAt: &now,
				})
			}
			if totalFee > 0 {
				act := totalFee
				if depositUsed < totalFee {
					act = depositUsed
				}
				tx.Create(&models.DepositRecord{
					OrderID:     order.ID,
					UserID:      order.UserID,
					Action:      models.DepositDeduct,
					Amount:      act,
					BeforeBal:   user.Balance,
					AfterBal:    user.Balance,
					TxnID:       utils.GenTxnID(),
					Status:      1,
					Reason:      fmt.Sprintf("丢失赔偿押金抵扣：%s，金额%d", order.OrderNo, act),
					ProcessedAt: &now,
				})
			}
		}
	}

	endTime := now
	order.Status = models.OrderLost
	order.EndTime = &endTime
	order.DurationSec = dur
	order.TotalFee = totalFee
	order.ExceptionFee = lostFee
	order.DepositStatus = 2
	order.RefundAmt = refundAmt
	if capHit {
		order.Remarks += " | 费用已封顶"
	}
	order.Remarks += fmt.Sprintf(" | 运营标记丢失：%s", req.Desc)
	if err := tx.Save(&order).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("更新订单失败")
	}

	tx.Model(&models.Battery{}).Where("id = ?", order.BatteryID).Updates(map[string]interface{}{
		"status":         models.BatteryLost,
		"last_report_at": now,
	})

	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("提交失败")
	}
	return nil
}

type HandleDamageReq struct {
	OrderNo   string `json:"order_no" binding:"required"`
	Operator  uint64 `json:"operator"`
	Evidence  string `json:"evidence"`
	Desc      string `json:"description"`
	FeeCustom int64  `json:"fee_custom"`
}

func (s *OpService) MarkDamage(ctx context.Context, req *HandleDamageReq) error {
	lockKey := fmt.Sprintf("op_order:%s", req.OrderNo)
	lock, acquired, err := redisx.AcquireLock(ctx, lockKey, 15*time.Second, 3)
	if err != nil {
		return fmt.Errorf("系统繁忙")
	}
	if !acquired {
		return fmt.Errorf("处理中")
	}
	defer lock.Release(ctx)

	var order models.RentalOrder
	if err := database.DB.Where("order_no = ?", req.OrderNo).First(&order).Error; err != nil {
		return fmt.Errorf("订单不存在")
	}
	if order.Status != models.OrderRenting && order.Status != models.OrderCompleted && order.Status != models.OrderDisputed {
		return fmt.Errorf("订单状态不允许")
	}

	tx := database.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var rule models.BillingRule
	if err := tx.First(&rule, order.RuleID).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("查询规则失败")
	}

	now := time.Now()
	damageFee := rule.DamageFee
	if req.FeeCustom > 0 {
		damageFee = req.FeeCustom
	}

	excep := models.ExceptionRecord{
		OrderID:     order.ID,
		UserID:      order.UserID,
		BatteryID:   order.BatteryID,
		ExcepType:   models.ExcepDamage,
		FeeAmt:      damageFee,
		FeeDeducted: 0,
		DepositUsed: 0,
		Status:      1,
		HandledBy:   &req.Operator,
		HandledAt:   &now,
		Evidence:    req.Evidence,
		Description: req.Desc,
	}
	if err := tx.Create(&excep).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("创建异常失败")
	}

	order.ExceptionFee += damageFee
	order.TotalFee += damageFee
	order.Remarks += fmt.Sprintf(" | 损坏扣费%d：%s", damageFee, req.Desc)
	if err := tx.Save(&order).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("更新订单失败")
	}

	tx.Model(&models.Battery{}).Where("id = ?", order.BatteryID).Updates(map[string]interface{}{
		"status": models.BatteryDamaged,
	})

	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("提交失败")
	}
	return nil
}

type HandleDisputeReq struct {
	DisputeID    uint64 `json:"dispute_id" binding:"required"`
	Operator     uint64 `json:"operator"`
	Decision     string `json:"decision" binding:"required"`
	AdjustFee    int64  `json:"adjust_fee"`
	RefundAmt    int64  `json:"refund_amt"`
	ReplyContent string `json:"reply_content"`
}

func (s *OpService) HandleDispute(ctx context.Context, req *HandleDisputeReq) error {
	lock, acquired, err := redisx.AcquireLock(ctx, fmt.Sprintf("dispute:%d", req.DisputeID), 15*time.Second, 3)
	if err != nil {
		return fmt.Errorf("系统繁忙")
	}
	if !acquired {
		return fmt.Errorf("处理中")
	}
	defer lock.Release(ctx)

	var dispute models.DisputeRecord
	if err := database.DB.First(&dispute, req.DisputeID).Error; err != nil {
		return fmt.Errorf("申诉不存在")
	}
	if dispute.Status != models.DisputeOpen && dispute.Status != models.DisputeReview {
		return fmt.Errorf("状态不允许处理")
	}

	tx := database.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	now := time.Now()

	if req.Decision == "resolve" || req.Decision == "partial" {
		var order models.RentalOrder
		if err := tx.Clauses(clauseUpdateLock).First(&order, dispute.OrderID).Error; err == nil {
			if req.AdjustFee > 0 {
				order.TotalFee -= req.AdjustFee
				if order.TotalFee < 0 {
					order.TotalFee = 0
				}
			}
			if req.RefundAmt > 0 {
				order.RefundAmt += req.RefundAmt
				var user models.User
				if err := tx.Clauses(clauseUpdateLock).First(&user, order.UserID).Error; err == nil {
					user.Balance += req.RefundAmt
					tx.Save(&user)
					tx.Create(&models.DepositRecord{
						OrderID:     order.ID,
						UserID:      order.UserID,
						Action:      models.DepositRefund,
						Amount:      req.RefundAmt,
						BeforeBal:   user.Balance - req.RefundAmt,
						AfterBal:    user.Balance,
						TxnID:       utils.GenTxnID(),
						Status:      1,
						Reason:      fmt.Sprintf("账单争议处理退款：申诉%d，原因：%s", dispute.ID, req.ReplyContent),
						ProcessedAt: &now,
					})
				}
			}
			tx.Save(&order)
		}
	}

	status := models.DisputeResolved
	if req.Decision == "reject" {
		status = models.DisputeRejected
	}

	updates := map[string]interface{}{
		"status":        status,
		"adjust_fee":    req.AdjustFee,
		"refund_amt":    req.RefundAmt,
		"handled_by":    &req.Operator,
		"handled_at":    &now,
		"reply_content": req.ReplyContent,
	}
	if err := tx.Model(&dispute).Updates(updates).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("更新申诉失败")
	}

	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("提交失败")
	}
	return nil
}

type CreateRepairReq struct {
	TargetType  string `json:"target_type" binding:"required"`
	TargetID    uint64 `json:"target_id" binding:"required"`
	FaultCode   string `json:"fault_code"`
	Description string `json:"description"`
}

func (s *OpService) CreateRepair(ctx context.Context, operator uint64, req *CreateRepairReq) (*models.RepairRecord, error) {
	rec := &models.RepairRecord{
		TargetType:  req.TargetType,
		TargetID:    req.TargetID,
		ReportBy:    &operator,
		Status:      models.RepairPending,
		FaultCode:   req.FaultCode,
		Description: req.Description,
	}
	if err := database.DB.Create(rec).Error; err != nil {
		return nil, fmt.Errorf("创建维修单失败")
	}

	if req.TargetType == "cabinet" {
		database.DB.Model(&models.Cabinet{}).Where("id = ?", req.TargetID).Update("status", models.CabinetMaintain)
	}
	return rec, nil
}

func (s *OpService) ResolveRepair(ctx context.Context, repairID uint64, operator uint64, status string, cost int64, remarks string) error {
	var repair models.RepairRecord
	if err := database.DB.First(&repair, repairID).Error; err != nil {
		return fmt.Errorf("维修单不存在")
	}
	now := time.Now()
	updates := map[string]interface{}{
		"status":    status,
		"repair_by": &operator,
		"finish_at": &now,
		"cost_amt":  cost,
		"remarks":   remarks,
	}
	if status == string(models.RepairFixing) {
		updates["start_at"] = &now
		delete(updates, "finish_at")
	}
	if err := database.DB.Model(&repair).Updates(updates).Error; err != nil {
		return fmt.Errorf("更新失败")
	}
	if status == string(models.RepairDone) && repair.TargetType == "cabinet" {
		database.DB.Model(&models.Cabinet{}).Where("id = ?", repair.TargetID).Update("status", models.CabinetOnline)
	}
	return nil
}
