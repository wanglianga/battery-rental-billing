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
	return s.saveReport(ctx, req.CabinetNo, req.ReportSeq, models.ReportHeartbeat, req.DeviceTime, req, nil, nil)
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
	if err != nil {
		return err
	}
	if req.LockType == "unlock" && req.LockResult == 1 {
		rental := NewRentalService()
		_ = rental.ConfirmUnlock(ctx, req.CabinetNo, req.SlotNo, true)
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

		report := models.CabinetReport{
			CabinetID:  0,
			ReportNo:   utils.GenReportNo("OFF", req.CabinetNo),
			ReportSeq:  r.ReportSeq,
			ReportType: models.ReportType(r.ReportType),
			DeviceTime: deviceTime,
			ServerTime: time.Now(),
			IsReplay:   true,
			Processed:  false,
			Payload:    r.Payload,
			SlotNo:     r.SlotNo,
			BatteryNo:  &r.BatteryNo,
		}

		var cabinet models.Cabinet
		if err := database.DB.Where("cabinet_no = ?", req.CabinetNo).First(&cabinet).Error; err == nil {
			report.CabinetID = cabinet.ID
		}

		if err := database.DB.Create(&report).Error; err != nil {
			skipped++
			continue
		}
		processed++

		s.processOfflineReport(ctx, req.CabinetNo, r)
	}

	return processed, skipped, nil
}

func (s *ReportService) processOfflineReport(ctx context.Context, cabinetNo string, r OfflineReportItem) {
	switch r.ReportType {
	case string(models.ReportBattery):
		var breq BatteryReportReq
		if err := json.Unmarshal([]byte(r.Payload), &breq); err == nil {
			breq.CabinetNo = cabinetNo
			_ = s.BatteryReport(ctx, &breq)
		}
	case string(models.ReportLock):
		var lreq LockReportReq
		if err := json.Unmarshal([]byte(r.Payload), &lreq); err == nil {
			lreq.CabinetNo = cabinetNo
			_ = s.LockReport(ctx, &lreq)
		}
	case string(models.ReportSlot):
		var sreq SlotStatusReq
		if err := json.Unmarshal([]byte(r.Payload), &sreq); err == nil {
			sreq.CabinetNo = cabinetNo
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
		if seq < lastSeqInt {
			return fmt.Errorf("乱序上报，已保存但不处理")
		}
		_ = redisx.SetEX(ctx, lastSeqKey, fmt.Sprintf("%d", seq), 30*24*time.Hour)
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
		Processed:  true,
		ProcessedAt: &now,
		Payload:    string(payloadBytes),
		SlotNo:     slotNo,
		BatteryNo:  batteryNo,
	}
	if err := database.DB.Create(&report).Error; err != nil {
		return fmt.Errorf("保存上报记录失败")
	}

	if rtype == models.ReportHeartbeat {
		database.DB.Model(&cabinet).Updates(map[string]interface{}{
			"status":         models.CabinetOnline,
			"heartbeat_at":   now,
			"last_online_at": now,
			"firmware_ver":   cabinet.FirmwareVer,
		})
	}
	return nil
}
