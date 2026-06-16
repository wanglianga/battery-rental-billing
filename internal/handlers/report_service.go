package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"battery-rental/internal/database"
	"battery-rental/internal/models"
	"battery-rental/internal/redisx"
	"battery-rental/internal/utils"

	"gorm.io/gorm"
)

type ReportService struct{}

func NewReportService() *ReportService {
	return &ReportService{}
}

type HeartbeatReq struct {
	CabinetNo   string  `json:"cabinet_no" binding:"required"`
	ReportSeq   int64   `json:"report_seq"`
	DeviceTime  string  `json:"device_time"`
	Temperature float64 `json:"temperature"`
	Voltage     float64 `json:"voltage"`
	Online      bool    `json:"online"`
	FirmwareVer string  `json:"firmware_ver"`
}

func (s *ReportService) Heartbeat(ctx context.Context, req *HeartbeatReq) error {
	err := s.saveReport(ctx, req.CabinetNo, req.ReportSeq, models.ReportHeartbeat, req.DeviceTime, req, nil, nil)
	if err != nil && err.Error() != "重复上报" && err.Error() != "乱序上报，已保存但不处理" {
		return err
	}

	if req.Online {
		var cabinet models.Cabinet
		if dbErr := database.DB.Where("cabinet_no = ?", req.CabinetNo).First(&cabinet).Error; dbErr == nil {
			if cabinet.Status == models.CabinetOffline {
				_ = database.DB.Model(&cabinet).Updates(map[string]interface{}{
					"status":         models.CabinetOnline,
					"last_online_at": time.Now(),
				})
				_ = s.handleCabinetRecovered(ctx, cabinet.ID)
			}
		}
	}
	return nil
}

func (s *ReportService) handleCabinetRecovered(ctx context.Context, cabinetID uint64) error {
	var abnormalOrders []models.RentalOrder
	database.DB.Where("from_cabinet_id = ? AND status = ? AND abnormal_reason = ?",
		cabinetID, models.OrderAbnormal, models.AbnormalReasonCabinetOffline).
		Order("id DESC").Find(&abnormalOrders)

	now := time.Now()
	for _, order := range abnormalOrders {
		if order.CompensationStatus != 0 {
			continue
		}

		if order.CreatedAt.Add(24 * time.Hour).Before(now) {
			_ = s.decideCompensationActionWithReason(ctx, &order, nil, models.CompensationManualReview, "柜机恢复超时（超过24小时），需人工核验")
			continue
		}

		var slot models.Slot
		slotExists := database.DB.First(&slot, order.FromSlotID).Error == nil

		var battery models.Battery
		batteryExists := false
		if order.BatteryID > 0 {
			batteryExists = database.DB.First(&battery, order.BatteryID).Error == nil
		}

		if !batteryExists || !slotExists {
			_ = s.decideCompensationActionWithReason(ctx, &order, nil, models.CompensationManualReview,
				fmt.Sprintf("数据缺失：slotExists=%v, batteryExists=%v", slotExists, batteryExists))
			continue
		}

		batteryTakenOut := false
		takeOutReason := ""
		if battery.Status == models.BatteryInUse {
			batteryTakenOut = true
			takeOutReason = "电池状态为in_use"
		} else if slot.Status == models.SlotEmpty {
			batteryTakenOut = true
			takeOutReason = "格口状态为empty"
		} else if slot.BatteryID == nil || *slot.BatteryID != battery.ID {
			batteryTakenOut = true
			takeOutReason = fmt.Sprintf("格口绑定电池ID=%v与异常单电池ID=%d不匹配", slot.BatteryID, battery.ID)
		}

		batteryStillInCabinet := slot.Status == models.SlotOccupied &&
			battery.Status == models.BatteryInCabinet &&
			slot.BatteryID != nil && *slot.BatteryID == battery.ID

		if batteryTakenOut {
			if order.CreatedAt.Add(1*time.Hour).Before(now) {
				_ = s.decideCompensationActionWithReason(ctx, &order, &battery, models.CompensationManualReview,
					fmt.Sprintf("柜机离线超过1小时后恢复，%s，电池可能已被他人取出，需人工核验", takeOutReason))
			} else {
				_ = s.decideCompensationActionWithReason(ctx, &order, &battery, models.CompensationRecreateOrder,
					fmt.Sprintf("柜机恢复在线，%s，判定电池确实被用户取出，自动补开订单", takeOutReason))
			}
		} else if batteryStillInCabinet {
			_ = s.decideCompensationActionWithReason(ctx, &order, &battery, models.CompensationRefundDeposit,
				"柜机恢复后电池仍在原格口，判定开柜失败未取走电池，全额退还押金")
		} else {
			_ = s.decideCompensationActionWithReason(ctx, &order, &battery, models.CompensationManualReview,
				fmt.Sprintf("状态矛盾：格口=%s, 格口电池ID=%v, 电池=%s, 电池ID=%d，无法自动判定",
					slot.Status, slot.BatteryID, battery.Status, battery.ID))
		}
	}
	return nil
}

type LockReportReq struct {
	CabinetNo  string `json:"cabinet_no" binding:"required"`
	SlotNo     int    `json:"slot_no" binding:"required"`
	ReportSeq  int64  `json:"report_seq"`
	DeviceTime string `json:"device_time"`
	LockStatus int    `json:"lock_status"`
	LockResult int    `json:"lock_result"`
	LockType   string `json:"lock_type"`
}

func (s *ReportService) LockReport(ctx context.Context, req *LockReportReq) error {
	err := s.saveReport(ctx, req.CabinetNo, req.ReportSeq, models.ReportLock, req.DeviceTime, req, &req.SlotNo, nil)
	if err != nil && err.Error() != "重复上报" && err.Error() != "乱序上报，已保存但不处理" {
		return err
	}

	var cabinet models.Cabinet
	if dbErr := database.DB.Where("cabinet_no = ?", req.CabinetNo).First(&cabinet).Error; dbErr != nil {
		return nil
	}
	var slot models.Slot
	database.DB.Where("cabinet_id = ? AND slot_no = ?", cabinet.ID, req.SlotNo).First(&slot)

	if req.LockType == "unlock" && req.LockResult == 1 {
		rental := NewRentalService()
		_ = rental.ConfirmUnlock(ctx, req.CabinetNo, req.SlotNo, true)
		if cabinet.ID > 0 && slot.ID > 0 {
			_ = s.handleUnlockSuccessForAbnormalOrders(ctx, cabinet.ID, slot.ID)
		}
	} else if req.LockType == "unlock" && req.LockResult != 1 {
		if cabinet.ID > 0 && slot.ID > 0 {
			_ = s.handleUnlockFailureForPendingOrders(ctx, cabinet.ID, slot.ID)
		}
	}
	return nil
}

func (s *ReportService) handleUnlockSuccessForAbnormalOrders(ctx context.Context, cabinetID uint64, slotID uint64) error {
	var orders []models.RentalOrder
	database.DB.Where("from_cabinet_id = ? AND from_slot_id = ? AND status = ?",
		cabinetID, slotID, models.OrderAbnormal).
		Order("id DESC").Find(&orders)

	now := time.Now()
	for _, order := range orders {
		if order.CompensationStatus != 0 {
			continue
		}
		if order.AbnormalReason != models.AbnormalReasonUnlockDoorNotOpen &&
			order.AbnormalReason != models.AbnormalReasonLockStuck &&
			order.AbnormalReason != models.AbnormalReasonBatteryMissing &&
			order.AbnormalReason != models.AbnormalReasonCabinetOffline {
			continue
		}

		if order.CreatedAt.Add(48 * time.Hour).Before(now) {
			_ = s.decideCompensationActionWithReason(ctx, &order, nil, models.CompensationManualReview,
				"门锁上报成功但异常单已超过48小时，需人工核验")
			continue
		}

		if order.BatteryID == 0 {
			_ = s.decideCompensationActionWithReason(ctx, &order, nil, models.CompensationRefundDeposit,
				"门锁上报成功但异常单无电池关联，判定为开柜失败全额退押金")
			continue
		}
		var battery models.Battery
		if database.DB.First(&battery, order.BatteryID).Error == nil {
			var slot models.Slot
			_ = database.DB.First(&slot, order.FromSlotID)

			batteryTakenOut := false
			takeOutReason := ""
			if battery.Status == models.BatteryInUse {
				batteryTakenOut = true
				takeOutReason = "电池状态为in_use"
			} else if slot.ID > 0 && slot.Status == models.SlotEmpty {
				batteryTakenOut = true
				takeOutReason = "格口状态为empty"
			} else if slot.ID > 0 && (slot.BatteryID == nil || *slot.BatteryID != battery.ID) {
				batteryTakenOut = true
				takeOutReason = fmt.Sprintf("格口绑定电池ID=%v与异常单电池ID=%d不匹配", slot.BatteryID, battery.ID)
			}

			batteryStillInCabinet := slot.ID > 0 &&
				slot.Status == models.SlotOccupied &&
				battery.Status == models.BatteryInCabinet &&
				slot.BatteryID != nil && *slot.BatteryID == battery.ID

			if order.CreatedAt.Add(1*time.Hour).Before(now) && batteryTakenOut {
				_ = s.decideCompensationActionWithReason(ctx, &order, &battery, models.CompensationManualReview,
					fmt.Sprintf("门锁上报成功但异常单超过1小时，%s，电池可能被他人取出或转移，需人工核验", takeOutReason))
			} else if batteryTakenOut {
				_ = s.decideCompensationActionWithReason(ctx, &order, &battery, models.CompensationRecreateOrder,
					fmt.Sprintf("门锁上报补传开柜成功，%s，判定用户已实际取走电池，自动补开订单开始计费", takeOutReason))
			} else if batteryStillInCabinet {
				_ = s.decideCompensationActionWithReason(ctx, &order, &battery, models.CompensationRefundDeposit,
					"门锁上报成功但电池仍在原格口，用户实际未取走电池，全额退还押金")
			} else {
				_ = s.decideCompensationActionWithReason(ctx, &order, &battery, models.CompensationManualReview,
					fmt.Sprintf("门锁上报成功但状态矛盾：格口=%s, 格口电池ID=%v, 电池=%s, 电池ID=%d，无法自动判定",
						slot.Status, slot.BatteryID, battery.Status, battery.ID))
			}
		} else {
			_ = s.decideCompensationActionWithReason(ctx, &order, nil, models.CompensationManualReview,
				"门锁上报成功但电池记录不存在，需人工核验")
		}
	}
	return nil
}

func (s *ReportService) handleUnlockFailureForPendingOrders(ctx context.Context, cabinetID uint64, slotID uint64) error {
	var orders []models.RentalOrder
	database.DB.Where("from_cabinet_id = ? AND from_slot_id = ? AND status IN ?",
		cabinetID, slotID, []models.OrderStatus{models.OrderPending, models.OrderRenting}).
		Order("id DESC").Find(&orders)

	now := time.Now()
	for _, order := range orders {
		if order.StartTime != nil && now.Sub(*order.StartTime) < 5*time.Minute {
			continue
		}
		orderID := order.ID
		reason := models.AbnormalReasonLockStuck
		excepType := models.ExcepLockStuck
		database.DB.Transaction(func(tx *gorm.DB) error {
			tx.Model(&models.RentalOrder{}).Where("id = ?", orderID).Updates(map[string]interface{}{
				"status":              models.OrderAbnormal,
				"abnormal_reason":     reason,
				"billing_enabled":     false,
				"compensation_action": models.CompensationNone,
				"compensation_status": 0,
				"start_time":          nil,
			})
			var o models.RentalOrder
			tx.First(&o, orderID)
			tx.Create(&models.ExceptionRecord{
				OrderID:       orderID,
				UserID:        o.UserID,
				BatteryID:     o.BatteryID,
				ExcepType:     excepType,
				FeeAmt:        0,
				FeeDeducted:   0,
				DepositUsed:   0,
				RefundAmt:     0,
				Status:        0,
				Description:   fmt.Sprintf("门锁上报开柜失败，自动转异常单：锁舌卡住/未弹开(%s)", reason),
			})
			return nil
		})
	}
	return nil
}

func (s *ReportService) decideCompensationAction(ctx context.Context, order *models.RentalOrder, battery *models.Battery, suggestedAction models.CompensationAction) error {
	defaultReason := ""
	switch suggestedAction {
	case models.CompensationRecreateOrder:
		if battery != nil {
			defaultReason = fmt.Sprintf("门锁上报开柜成功，电池确实已取出(%s)，自动补开订单开始计费", battery.BatteryNo)
		} else {
			defaultReason = "门锁上报开柜成功，自动补开订单开始计费"
		}
	case models.CompensationRefundDeposit:
		defaultReason = fmt.Sprintf("电池仍在柜中(%s)，押金全额退还", order.OrderNo)
	case models.CompensationManualReview:
		defaultReason = "无法自动判断，转人工核验"
	}
	return s.decideCompensationActionWithReason(ctx, order, battery, suggestedAction, defaultReason)
}

func (s *ReportService) decideCompensationActionWithReason(ctx context.Context, order *models.RentalOrder, battery *models.Battery, suggestedAction models.CompensationAction, reason string) error {
	lockKey := fmt.Sprintf("comp:%d", order.ID)
	lock, acquired, err := redisx.AcquireLock(ctx, lockKey, 10*time.Second, 3)
	if err != nil || !acquired {
		return fmt.Errorf("补偿处理中，请稍后再试")
	}
	defer lock.Release(ctx)

	var freshOrder models.RentalOrder
	if err := database.DB.First(&freshOrder, order.ID).Error; err != nil {
		return err
	}
	if freshOrder.CompensationStatus != 0 && freshOrder.CompensationStatus != 2 {
		return fmt.Errorf("该订单已处理过补偿")
	}

	now := time.Now()
	tx := database.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	action := suggestedAction
	remark := reason
	orderStatus := freshOrder.Status
	billingEnabled := freshOrder.BillingEnabled
	depositStatus := freshOrder.DepositStatus
	refundAmt := freshOrder.RefundAmt
	compensationStatus := 0
	var startTime *time.Time

	switch action {
	case models.CompensationRecreateOrder:
		if battery != nil {
			var batCheck models.Battery
			batteryOK := tx.First(&batCheck, battery.ID).Error == nil
			if batteryOK && (batCheck.Status == models.BatteryInUse || batCheck.Status == models.BatteryLost || batCheck.Status == models.BatteryDamaged) {
				orderStatus = models.OrderRenting
				billingEnabled = true
				startTime = &now
				compensationStatus = 1
				remark = fmt.Sprintf("[补开订单] %s | 电池状态校验通过：%s", reason, batCheck.Status)
			} else if batteryOK {
				action = models.CompensationManualReview
				remark = fmt.Sprintf("[补开失败] %s | 电池状态=%s不符合补开条件，转人工", reason, batCheck.Status)
			} else {
				action = models.CompensationManualReview
				remark = fmt.Sprintf("[补开失败] %s | 电池查询失败，转人工", reason)
			}
		} else if freshOrder.BatteryID > 0 {
			var batCheck models.Battery
			batteryOK := tx.First(&batCheck, freshOrder.BatteryID).Error == nil
			if batteryOK && batCheck.Status == models.BatteryInUse {
				orderStatus = models.OrderRenting
				billingEnabled = true
				startTime = &now
				compensationStatus = 1
				remark = fmt.Sprintf("[补开订单] %s | 电池校验通过：%s", reason, batCheck.BatteryNo)
			} else {
				action = models.CompensationManualReview
				remark = fmt.Sprintf("[补开失败] %s | 电池校验不通过，转人工", reason)
			}
		} else {
			action = models.CompensationManualReview
			remark = fmt.Sprintf("[补开失败] %s | 无电池关联，转人工", reason)
		}
	case models.CompensationRefundDeposit:
		if freshOrder.DepositStatus == 1 && freshOrder.DepositAmt > 0 {
			var user models.User
			if err := tx.Clauses(clauseUpdateLock).First(&user, freshOrder.UserID).Error; err == nil {
				toRefund := freshOrder.DepositAmt
				beforeBal := user.Balance
				user.Balance += toRefund
				if err := tx.Save(&user).Error; err == nil {
					txnID := utils.GenTxnID()
					tx.Create(&models.DepositRecord{
						OrderID:     freshOrder.ID,
						UserID:      freshOrder.UserID,
						Action:      models.DepositRelease,
						Amount:      toRefund,
						BeforeBal:   beforeBal,
						AfterBal:    user.Balance,
						TxnID:       txnID,
						Status:      1,
						Reason:      fmt.Sprintf("开柜失败补偿押金全额退还：订单%s | 原因：%s", freshOrder.OrderNo, reason),
						ProcessedAt: &now,
					})
					depositStatus = 2
					refundAmt = toRefund
					orderStatus = models.OrderAbnormalCompensated
					compensationStatus = 1
					remark = fmt.Sprintf("[退押金成功] %s | 退还%d分，用户余额 %d → %d", reason, toRefund, beforeBal, user.Balance)
				} else {
					action = models.CompensationManualReview
					remark = fmt.Sprintf("[退押金失败] %s | 更新用户余额失败：%v，转人工", reason, err)
				}
			} else {
				action = models.CompensationManualReview
				remark = fmt.Sprintf("[退押金失败] %s | 查询用户失败：%v，转人工", reason, err)
			}
		} else {
			orderStatus = models.OrderAbnormalCompensated
			compensationStatus = 1
			if freshOrder.DepositAmt == 0 {
				remark = fmt.Sprintf("[免押金] %s | 无押金需退还，异常单直接完结", reason)
			} else if freshOrder.DepositStatus != 1 {
				remark = fmt.Sprintf("[押金状态] %s | 押金状态=%d非冻结状态，异常单直接完结", reason, freshOrder.DepositStatus)
			}
		}
	case models.CompensationManualReview:
		compensationStatus = 2
		if remark == "" {
			remark = "无法自动判断，转人工核验"
		} else {
			remark = "[转人工] " + remark
		}
	default:
		action = models.CompensationManualReview
		compensationStatus = 2
		remark = fmt.Sprintf("[未知动作] 收到action=%s，默认转人工核验", action)
	}

	updates := map[string]interface{}{
		"status":              orderStatus,
		"billing_enabled":     billingEnabled,
		"compensation_action": action,
		"compensation_status": compensationStatus,
		"deposit_status":      depositStatus,
		"refund_amt":          refundAmt,
		"compensation_remark": remark,
		"compensated_at":      &now,
	}
	if startTime != nil {
		updates["start_time"] = startTime
	}
	if err := tx.Model(&models.RentalOrder{}).Where("id = ?", freshOrder.ID).Updates(updates).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("更新订单失败：%w", err)
	}

	excepStatus := 1
	if action == models.CompensationManualReview {
		excepStatus = 0
	}
	tx.Model(&models.ExceptionRecord{}).Where("order_id = ?", freshOrder.ID).
		Updates(map[string]interface{}{
			"status":      excepStatus,
			"handled_at":  &now,
			"description": gorm.Expr("CONCAT(description, ?)", " | [补偿处理] "+remark),
		})

	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("提交事务失败：%w", err)
	}
	return nil
}

type BatteryReportReq struct {
	CabinetNo   string  `json:"cabinet_no" binding:"required"`
	SlotNo      int     `json:"slot_no"`
	BatteryNo   string  `json:"battery_no"`
	ReportSeq   int64   `json:"report_seq"`
	DeviceTime  string  `json:"device_time"`
	SOC         int     `json:"soc"`
	Temperature float64 `json:"temperature"`
	Voltage     float64 `json:"voltage"`
	Current     float64 `json:"current"`
	Health      int     `json:"health"`
}

func (s *ReportService) BatteryReport(ctx context.Context, req *BatteryReportReq) error {
	err := s.saveReport(ctx, req.CabinetNo, req.ReportSeq, models.ReportBattery, req.DeviceTime, req, &req.SlotNo, &req.BatteryNo)
	if err != nil {
		return err
	}

	now := time.Now()
	if req.BatteryNo != "" {
		database.DB.Model(&models.Battery{}).Where("battery_no = ?", req.BatteryNo).Updates(map[string]interface{}{
			"soc":            req.SOC,
			"temperature":    req.Temperature,
			"last_report_at": now,
		})
	}
	return nil
}

type SlotStatusReq struct {
	CabinetNo  string             `json:"cabinet_no" binding:"required"`
	ReportSeq  int64              `json:"report_seq"`
	DeviceTime string             `json:"device_time"`
	Slots      []SlotStatusDetail `json:"slots"`
}

type SlotStatusDetail struct {
	SlotNo      int    `json:"slot_no"`
	Occupied    bool   `json:"occupied"`
	BatteryNo   string `json:"battery_no"`
	LockStatus  int    `json:"lock_status"`
	DoorStatus  int    `json:"door_status"`
	SOC         int    `json:"soc"`
}

func (s *ReportService) SlotReport(ctx context.Context, req *SlotStatusReq) error {
	err := s.saveReport(ctx, req.CabinetNo, req.ReportSeq, models.ReportSlot, req.DeviceTime, req, nil, nil)
	if err != nil {
		return err
	}

	var cabinet models.Cabinet
	if err := database.DB.Where("cabinet_no = ?", req.CabinetNo).First(&cabinet).Error; err != nil {
		return nil
	}

	now := time.Now()
	for _, sd := range req.Slots {
		var slot models.Slot
		err := database.DB.Where("cabinet_id = ? AND slot_no = ?", cabinet.ID, sd.SlotNo).First(&slot).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				var bat *models.Battery
				if sd.BatteryNo != "" {
					var b models.Battery
					if err := database.DB.Where("battery_no = ?", sd.BatteryNo).First(&b).Error; err == nil {
						bat = &b
					}
				}
				ns := models.Slot{
					CabinetID:  cabinet.ID,
					SlotNo:     sd.SlotNo,
					Status:     s.detectSlotStatus(sd.Occupied),
					LockStatus: sd.LockStatus,
					LastAction: &now,
				}
				if bat != nil {
					ns.BatteryID = &bat.ID
					bat.SlotID = &ns.ID
				}
				database.DB.Create(&ns)
			}
			continue
		}

		updates := map[string]interface{}{
			"lock_status": sd.LockStatus,
		}
		newStatus := s.detectSlotStatus(sd.Occupied)
		if slot.Status == models.SlotUnlocking || slot.Status == models.SlotReturning {
		} else {
			updates["status"] = newStatus
		}

		if sd.BatteryNo != "" {
			var b models.Battery
			if err := database.DB.Where("battery_no = ?", sd.BatteryNo).First(&b).Error; err == nil {
				updates["battery_id"] = b.ID
				slot.BatteryID = &b.ID
				b.SlotID = &slot.ID
				b.SOC = sd.SOC
				b.LastReportAt = &now
				database.DB.Save(&b)
			}
		} else {
			updates["battery_id"] = nil
		}

		database.DB.Model(&slot).Updates(updates)
	}
	return nil
}

func (s *ReportService) detectSlotStatus(occupied bool) models.SlotStatus {
	if occupied {
		return models.SlotOccupied
	}
	return models.SlotEmpty
}

type OfflineBatchReq struct {
	CabinetNo string              `json:"cabinet_no" binding:"required"`
	Reports   []OfflineReportItem `json:"reports"`
}

type OfflineReportItem struct {
	ReportSeq  int64  `json:"report_seq"`
	ReportType string `json:"report_type"`
	DeviceTime string `json:"device_time"`
	Payload    string `json:"payload"`
	SlotNo     *int   `json:"slot_no"`
	BatteryNo  string `json:"battery_no"`
}

func (s *ReportService) OfflineReplay(ctx context.Context, req *OfflineBatchReq) (int, int, error) {
	if len(req.Reports) == 0 {
		return 0, 0, nil
	}

	sort.Slice(req.Reports, func(i, j int) bool {
		return req.Reports[i].ReportSeq < req.Reports[j].ReportSeq
	})

	processed := 0
	skipped := 0

	lastSeqKey := fmt.Sprintf("last_seq:%s", req.CabinetNo)
	lastSeqStr, _ := redisx.Get(ctx, lastSeqKey)
	lastSeqInt := int64(0)
	if lastSeqStr != "" {
		fmt.Sscanf(lastSeqStr, "%d", &lastSeqInt)
	}

	for _, r := range req.Reports {
		dedupKey := fmt.Sprintf("offline_rpt:%s:%d", req.CabinetNo, r.ReportSeq)
		ok, err := redisx.SetNXWithTTL(ctx, dedupKey, "1", 30*24*time.Hour)
		if err != nil {
			skipped++
			continue
		}
		if !ok {
			skipped++
			continue
		}

		deviceTime, _ := time.Parse(time.RFC3339, r.DeviceTime)
		if deviceTime.IsZero() {
			deviceTime = time.Now()
		}

		isProcessed := false
		if r.ReportSeq > lastSeqInt {
			isProcessed = true
			lastSeqInt = r.ReportSeq
			_ = redisx.SetEX(ctx, lastSeqKey, fmt.Sprintf("%d", lastSeqInt), 30*24*time.Hour)
		}

		now := time.Now()
		report := models.CabinetReport{
			CabinetID:  0,
			ReportNo:   utils.GenReportNo("OFF", req.CabinetNo),
			ReportSeq:  r.ReportSeq,
			ReportType: models.ReportType(r.ReportType),
			DeviceTime: deviceTime,
			ServerTime: now,
			IsReplay:   true,
			Processed:  isProcessed,
			Payload:    r.Payload,
			SlotNo:     r.SlotNo,
			BatteryNo:  &r.BatteryNo,
		}

		if isProcessed {
			report.ProcessedAt = &now
		}

		var cabinet models.Cabinet
		if err := database.DB.Where("cabinet_no = ?", req.CabinetNo).First(&cabinet).Error; err == nil {
			report.CabinetID = cabinet.ID
		}

		if err := database.DB.Create(&report).Error; err != nil {
			skipped++
			continue
		}

		if isProcessed {
			processed++
			s.processOfflineReportDirect(ctx, req.CabinetNo, r)
		} else {
			skipped++
		}
	}

	if processed > 0 {
		s.handleOfflineReplayCabinetRecovery(ctx, req.CabinetNo)
	}

	return processed, skipped, nil
}

func (s *ReportService) handleOfflineReplayCabinetRecovery(ctx context.Context, cabinetNo string) {
	var cabinet models.Cabinet
	if err := database.DB.Where("cabinet_no = ?", cabinetNo).First(&cabinet).Error; err != nil {
		return
	}
	if cabinet.Status == models.CabinetOffline {
		now := time.Now()
		_ = database.DB.Model(&cabinet).Updates(map[string]interface{}{
			"status":         models.CabinetOnline,
			"last_online_at": now,
			"heartbeat_at":   now,
		})
		_ = s.handleCabinetRecovered(ctx, cabinet.ID)
	}
}

func (s *ReportService) processOfflineReportDirect(ctx context.Context, cabinetNo string, r OfflineReportItem) {
	switch r.ReportType {
	case string(models.ReportBattery):
		var breq BatteryReportReq
		if err := json.Unmarshal([]byte(r.Payload), &breq); err == nil {
			now := time.Now()
			batteryNo := r.BatteryNo
			if batteryNo == "" {
				batteryNo = breq.BatteryNo
			}
			if batteryNo != "" {
				database.DB.Model(&models.Battery{}).Where("battery_no = ?", batteryNo).Updates(map[string]interface{}{
					"soc":            breq.SOC,
					"temperature":    breq.Temperature,
					"last_report_at": now,
				})
			}
		}
	case string(models.ReportLock):
		var lreq LockReportReq
		if err := json.Unmarshal([]byte(r.Payload), &lreq); err == nil {
			slotNo := lreq.SlotNo
			if slotNo == 0 && r.SlotNo != nil {
				slotNo = *r.SlotNo
			}
			if lreq.LockType == "unlock" && lreq.LockResult == 1 {
				if cabinetNo != "" && slotNo > 0 {
					rental := NewRentalService()
					_ = rental.ConfirmUnlock(ctx, cabinetNo, slotNo, true)
				}
				var cabinet models.Cabinet
				if dbErr := database.DB.Where("cabinet_no = ?", cabinetNo).First(&cabinet).Error; dbErr == nil {
					var slot models.Slot
					if slotErr := database.DB.Where("cabinet_id = ? AND slot_no = ?", cabinet.ID, slotNo).First(&slot).Error; slotErr == nil {
						_ = s.handleUnlockSuccessForAbnormalOrders(ctx, cabinet.ID, slot.ID)
					}
				}
			} else if lreq.LockType == "unlock" && lreq.LockResult != 1 {
				if cabinetNo != "" && slotNo > 0 {
					var cabinet models.Cabinet
					if dbErr := database.DB.Where("cabinet_no = ?", cabinetNo).First(&cabinet).Error; dbErr == nil {
						var slot models.Slot
						if slotErr := database.DB.Where("cabinet_id = ? AND slot_no = ?", cabinet.ID, slotNo).First(&slot).Error; slotErr == nil {
							_ = s.handleUnlockFailureForPendingOrders(ctx, cabinet.ID, slot.ID)
						}
					}
				}
			}
		}
	case string(models.ReportSlot):
		var sreq SlotStatusReq
		if err := json.Unmarshal([]byte(r.Payload), &sreq); err == nil {
			if sreq.CabinetNo == "" {
				sreq.CabinetNo = cabinetNo
			}
			_ = s.SlotReport(ctx, &sreq)
		}
	}
}

func (s *ReportService) saveReport(ctx context.Context, cabinetNo string, seq int64, rtype models.ReportType, deviceTimeStr string, payload interface{}, slotNo *int, batteryNo *string) error {
	var cabinet models.Cabinet
	if err := database.DB.Where("cabinet_no = ?", cabinetNo).First(&cabinet).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("柜机未注册：%s", cabinetNo)
		}
		return fmt.Errorf("查询柜机失败")
	}

	isProcessed := true

	if seq > 0 {
		dedupKey := fmt.Sprintf("rpt:%s:%d", cabinetNo, seq)
		ok, err := redisx.SetNXWithTTL(ctx, dedupKey, "1", 7*24*time.Hour)
		if err != nil {
			return fmt.Errorf("系统繁忙")
		}
		if !ok {
			return fmt.Errorf("重复上报")
		}

		lastSeqKey := fmt.Sprintf("last_seq:%s", cabinetNo)
		lastSeq, _ := redisx.Get(ctx, lastSeqKey)
		lastSeqInt := int64(0)
		if lastSeq != "" {
			fmt.Sscanf(lastSeq, "%d", &lastSeqInt)
		}
		if seq <= lastSeqInt {
			isProcessed = false
		} else {
			_ = redisx.SetEX(ctx, lastSeqKey, fmt.Sprintf("%d", seq), 30*24*time.Hour)
		}
	}

	deviceTime, _ := time.Parse(time.RFC3339, deviceTimeStr)
	if deviceTime.IsZero() {
		deviceTime = time.Now()
	}

	payloadBytes, _ := json.Marshal(payload)
	now := time.Now()

	report := models.CabinetReport{
		CabinetID:  cabinet.ID,
		ReportNo:   utils.GenReportNo(string(rtype), cabinetNo),
		ReportSeq:  seq,
		ReportType: rtype,
		DeviceTime: deviceTime,
		ServerTime: now,
		IsReplay:   false,
		Processed:  isProcessed,
		Payload:    string(payloadBytes),
		SlotNo:     slotNo,
		BatteryNo:  batteryNo,
	}

	if isProcessed {
		report.ProcessedAt = &now
	}

	if err := database.DB.Create(&report).Error; err != nil {
		return fmt.Errorf("保存上报记录失败")
	}

	if rtype == models.ReportHeartbeat && isProcessed {
		database.DB.Model(&cabinet).Updates(map[string]interface{}{
			"status":         models.CabinetOnline,
			"heartbeat_at":   now,
			"last_online_at": now,
			"firmware_ver":   cabinet.FirmwareVer,
		})
	}

	if !isProcessed {
		return fmt.Errorf("乱序上报，已保存但不处理")
	}

	return nil
}
