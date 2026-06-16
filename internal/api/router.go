package api

import (
	"fmt"
	"net/http"
	"time"

	"battery-rental/internal/auth"
	"battery-rental/internal/config"
	"battery-rental/internal/database"
	"battery-rental/internal/handlers"
	"battery-rental/internal/middleware"
	"battery-rental/internal/models"
	"battery-rental/internal/utils"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm/clause"
)

type Router struct {
	rental  *handlers.RentalService
	returnS *handlers.ReturnService
	report  *handlers.ReportService
	payment *handlers.PaymentService
	op      *handlers.OpService
}

func NewRouter() *Router {
	return &Router{
		rental:  handlers.NewRentalService(),
		returnS: handlers.NewReturnService(),
		report:  handlers.NewReportService(),
		payment: handlers.NewPaymentService(),
		op:      handlers.NewOpService(),
	}
}

func (r *Router) RegisterRoutes(e *gin.Engine) {
	api := e.Group("/api/v1")

	api.GET("/health", func(c *gin.Context) {
		utils.OK(c, gin.H{"status": "ok", "time": time.Now().Unix()})
	})

	authG := api.Group("/auth")
	{
		authG.POST("/register", r.Register)
		authG.POST("/login", r.Login)
		authG.POST("/logout", middleware.AuthRequired(), r.Logout)
	}

	user := api.Group("/user")
	user.Use(middleware.AuthRequired(string(models.RoleCustomer), string(models.RoleAdmin), string(models.RoleOperator)))
	{
		user.GET("/profile", r.GetProfile)
		user.GET("/balance", r.GetBalance)
		user.POST("/scan-rent", middleware.Idempotent(3600), r.ScanRent)
		user.GET("/active-order", r.GetActiveOrder)
		user.GET("/orders", r.ListOrders)
		user.GET("/orders/:id", r.GetOrderDetail)
		user.POST("/dispute", r.CreateDispute)
		user.GET("/disputes", r.ListDisputes)
		user.POST("/report-unlock-failure", middleware.Idempotent(3600), r.ReportUnlockFailure)
	}

	pay := api.Group("/pay")
	pay.Use(middleware.AuthRequired())
	{
		pay.POST("/recharge", middleware.Idempotent(3600), r.Recharge)
		pay.GET("/records", r.PayRecords)
	}
	api.POST("/pay/callback", middleware.Idempotent(86400), r.PayCallback)
	api.GET("/pay/mock/:pay_no", r.MockPay)

	device := api.Group("/device")
	device.Use(middleware.DeviceAuth())
	{
		device.POST("/heartbeat", r.Heartbeat)
		device.POST("/lock-report", r.LockReport)
		device.POST("/battery-report", r.BatteryReport)
		device.POST("/slot-report", r.SlotReport)
		device.POST("/offline-replay", r.OfflineReplay)
		device.POST("/detect-return", r.DetectReturn)
	}

	admin := api.Group("/admin")
	admin.Use(middleware.AuthRequired(string(models.RoleAdmin), string(models.RoleOperator)))
	{
		admin.GET("/cabinets", r.ListCabinets)
		admin.POST("/cabinets", r.CreateCabinet)
		admin.GET("/cabinets/:id", r.GetCabinet)
		admin.PUT("/cabinets/:id", r.UpdateCabinet)
		admin.GET("/batteries", r.ListBatteries)
		admin.POST("/batteries", r.CreateBattery)
		admin.POST("/batteries/assign", r.AssignBattery)
		admin.GET("/rules", r.ListRules)
		admin.POST("/rules", r.CreateRule)
		admin.PUT("/rules/:id", r.UpdateRule)
		admin.GET("/orders", r.AdminListOrders)
		admin.POST("/orders/manual-return", r.ManualReturn)
		admin.POST("/orders/mark-lost", r.MarkLost)
		admin.POST("/orders/mark-damage", r.MarkDamage)
		admin.GET("/disputes", r.AdminListDisputes)
		admin.POST("/disputes/handle", r.HandleDispute)
		admin.GET("/repairs", r.ListRepairs)
		admin.POST("/repairs", r.CreateRepair)
		admin.PUT("/repairs/:id", r.UpdateRepair)
		admin.GET("/exceptions", r.ListExceptions)
		admin.GET("/reports", r.ListReports)
		admin.POST("/slots/batch-create", r.BatchCreateSlots)
		admin.GET("/return-records", r.ListReturnRecords)
		admin.GET("/return-records/:id", r.GetReturnRecordDetail)
		admin.POST("/orders/handle-compensation", r.HandleAbnormalCompensation)
		admin.GET("/abnormal-orders", r.ListAbnormalOrders)
		admin.GET("/abnormal-orders/:id", r.GetAbnormalOrderDetail)
	}
}

type RegisterReq struct {
	Phone    string `json:"phone" binding:"required"`
	Nickname string `json:"nickname"`
	Password string `json:"password" binding:"required,min=6"`
	Code     string `json:"code"`
}

func (r *Router) Register(c *gin.Context) {
	var req RegisterReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误："+err.Error())
		return
	}
	var cnt int64
	database.DB.Model(&models.User{}).Where("phone = ?", req.Phone).Count(&cnt)
	if cnt > 0 {
		utils.Fail(c, 409, "手机号已注册")
		return
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	user := models.User{
		Phone:        req.Phone,
		Nickname:     req.Nickname,
		PasswordHash: string(hash),
		Role:         models.RoleCustomer,
		Balance:      0,
		DepositFree:  false,
		Status:       1,
	}
	if err := database.DB.Create(&user).Error; err != nil {
		utils.Fail(c, 500, "注册失败")
		return
	}
	token, exp, _ := auth.GenerateToken(user.ID, user.Phone, string(user.Role))
	utils.OK(c, gin.H{
		"user_id":    user.ID,
		"token":      token,
		"expires_at": exp,
		"role":       user.Role,
	})
}

type LoginReq struct {
	Phone    string `json:"phone" binding:"required"`
	Password string `json:"password" binding:"required"`
}

func (r *Router) Login(c *gin.Context) {
	var req LoginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	var user models.User
	if err := database.DB.Where("phone = ?", req.Phone).First(&user).Error; err != nil {
		utils.Fail(c, 401, "账号或密码错误")
		return
	}
	if user.Status != 1 {
		utils.Fail(c, 403, "账号已禁用")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		utils.Fail(c, 401, "账号或密码错误")
		return
	}
	token, exp, _ := auth.GenerateToken(user.ID, user.Phone, string(user.Role))
	utils.OK(c, gin.H{
		"user_id":    user.ID,
		"nickname":   user.Nickname,
		"phone":      user.Phone,
		"role":       user.Role,
		"balance":    user.Balance,
		"token":      token,
		"expires_at": exp,
		"deposit":    config.AppConfig.DepositAmt,
	})
}

func (r *Router) Logout(c *gin.Context) {
	authHeader := c.GetHeader("Authorization")
	if len(authHeader) > 7 {
		token := authHeader[7:]
		_ = utils.ToString(token)
	}
	utils.OK(c, nil)
}

func (r *Router) GetProfile(c *gin.Context) {
	uid := middleware.GetUserID(c)
	var user models.User
	if err := database.DB.Select("id, phone, nickname, role, balance, deposit_free, status, created_at").
		First(&user, uid).Error; err != nil {
		utils.Fail(c, 404, "用户不存在")
		return
	}
	utils.OK(c, user)
}

func (r *Router) GetBalance(c *gin.Context) {
	uid := middleware.GetUserID(c)
	var user models.User
	database.DB.Select("balance").First(&user, uid)
	var depRecords []models.DepositRecord
	database.DB.Where("user_id = ?", uid).Order("id DESC").Limit(10).Find(&depRecords)
	utils.OK(c, gin.H{
		"balance":  user.Balance,
		"deposit":  config.AppConfig.DepositAmt,
		"records":  depRecords,
	})
}

func (r *Router) ScanRent(c *gin.Context) {
	var req handlers.ScanRentalReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误："+err.Error())
		return
	}
	uid := middleware.GetUserID(c)
	resp, err := r.rental.ScanAndRent(c.Request.Context(), uid, &req)
	if err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, resp)
}

func (r *Router) GetActiveOrder(c *gin.Context) {
	uid := middleware.GetUserID(c)
	order, err := r.returnS.GetActiveOrder(c.Request.Context(), uid)
	if err != nil {
		utils.Fail(c, 500, err.Error())
		return
	}
	if order == nil {
		utils.OK(c, nil)
		return
	}
	utils.OK(c, order)
}

func (r *Router) ListOrders(c *gin.Context) {
	uid := middleware.GetUserID(c)
	page := utils.ParseInt(c.DefaultQuery("page", "1"))
	size := utils.ParseInt(c.DefaultQuery("size", "20"))
	list, total, err := r.returnS.ListOrders(c.Request.Context(), uid, page, size)
	if err != nil {
		utils.Fail(c, 500, err.Error())
		return
	}
	utils.OK(c, gin.H{"list": list, "total": total, "page": page, "size": size})
}

func (r *Router) GetOrderDetail(c *gin.Context) {
	id := utils.ParseUint(c.Param("id"))
	uid := middleware.GetUserID(c)
	var order models.RentalOrder
	if err := database.DB.Preload("Battery").Preload("Rule").
		Where("id = ? AND user_id = ?", id, uid).First(&order).Error; err != nil {
		utils.Fail(c, 404, "订单不存在")
		return
	}
	var returns []models.ReturnRecord
	database.DB.Where("order_id = ?", id).Find(&returns)
	var exceptions []models.ExceptionRecord
	database.DB.Where("order_id = ?", id).Find(&exceptions)
	var disputes []models.DisputeRecord
	database.DB.Where("order_id = ?", id).Find(&disputes)
	var pays []models.PaymentRecord
	database.DB.Where("order_id = ?", id).Find(&pays)
	var depos []models.DepositRecord
	database.DB.Where("order_id = ?", id).Find(&depos)
	utils.OK(c, gin.H{
		"order":     order,
		"returns":   returns,
		"exceptions": exceptions,
		"disputes":  disputes,
		"payments":  pays,
		"deposits":  depos,
	})
}

type CreateDisputeReq struct {
	OrderID uint64 `json:"order_id" binding:"required"`
	Title   string `json:"title" binding:"required"`
	Content string `json:"content" binding:"required"`
	Fee     int64  `json:"filed_fee"`
}

func (r *Router) CreateDispute(c *gin.Context) {
	var req CreateDisputeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	uid := middleware.GetUserID(c)
	var order models.RentalOrder
	if err := database.DB.Where("id = ? AND user_id = ?", req.OrderID, uid).First(&order).Error; err != nil {
		utils.Fail(c, 404, "订单不存在")
		return
	}
	dispute := models.DisputeRecord{
		OrderID:    req.OrderID,
		UserID:     uid,
		Title:      req.Title,
		Content:    req.Content,
		Status:     models.DisputeOpen,
		FiledFee:   req.Fee,
		AdjustFee:  0,
		RefundAmt:  0,
	}
	if err := database.DB.Create(&dispute).Error; err != nil {
		utils.Fail(c, 500, "提交失败")
		return
	}
	database.DB.Model(&order).Update("status", models.OrderDisputed)
	utils.OK(c, dispute)
}

func (r *Router) ListDisputes(c *gin.Context) {
	uid := middleware.GetUserID(c)
	var list []models.DisputeRecord
	database.DB.Where("user_id = ?", uid).Order("id DESC").Find(&list)
	utils.OK(c, list)
}

func (r *Router) Recharge(c *gin.Context) {
	var req handlers.RechargeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	if req.PayType == "" {
		req.PayType = "mock"
	}
	uid := middleware.GetUserID(c)
	resp, err := r.payment.Recharge(c.Request.Context(), uid, &req)
	if err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, resp)
}

func (r *Router) PayRecords(c *gin.Context) {
	uid := middleware.GetUserID(c)
	var list []models.PaymentRecord
	database.DB.Where("user_id = ?", uid).Order("id DESC").Limit(50).Find(&list)
	utils.OK(c, list)
}

func (r *Router) PayCallback(c *gin.Context) {
	var req handlers.PayCallbackReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	resp, err := r.payment.HandleCallback(c.Request.Context(), &req)
	if err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, resp)
}

func (r *Router) MockPay(c *gin.Context) {
	payNo := c.Param("pay_no")
	resp, err := r.payment.MockPay(c.Request.Context(), payNo)
	if err != nil {
		c.String(http.StatusOK, "支付失败："+err.Error())
		return
	}
	c.String(http.StatusOK, "支付结果：%+v", resp)
}

func (r *Router) Heartbeat(c *gin.Context) {
	var req handlers.HeartbeatReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	if err := r.report.Heartbeat(c.Request.Context(), &req); err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, gin.H{"server_time": time.Now().Unix()})
}

func (r *Router) LockReport(c *gin.Context) {
	var req handlers.LockReportReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	if err := r.report.LockReport(c.Request.Context(), &req); err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, nil)
}

func (r *Router) BatteryReport(c *gin.Context) {
	var req handlers.BatteryReportReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	if err := r.report.BatteryReport(c.Request.Context(), &req); err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, nil)
}

func (r *Router) SlotReport(c *gin.Context) {
	var req handlers.SlotStatusReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	if err := r.report.SlotReport(c.Request.Context(), &req); err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, nil)
}

func (r *Router) OfflineReplay(c *gin.Context) {
	var req handlers.OfflineBatchReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	processed, skipped, err := r.report.OfflineReplay(c.Request.Context(), &req)
	if err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, gin.H{"processed": processed, "skipped": skipped})
}

func (r *Router) DetectReturn(c *gin.Context) {
	var req handlers.DetectBatteryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	resp, err := r.returnS.DetectBatteryReturn(c.Request.Context(), &req)
	if err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, resp)
}

type CreateCabinetReq struct {
	CabinetNo string  `json:"cabinet_no" binding:"required"`
	Name      string  `json:"name" binding:"required"`
	Address   string  `json:"address"`
	Longitude float64 `json:"longitude"`
	Latitude  float64 `json:"latitude"`
	TotalSlots int    `json:"total_slots"`
}

func (r *Router) ListCabinets(c *gin.Context) {
	var list []models.Cabinet
	q := database.DB.Model(&models.Cabinet{})
	if kw := c.Query("keyword"); kw != "" {
		q = q.Where("cabinet_no LIKE ? OR name LIKE ?", "%"+kw+"%", "%"+kw+"%")
	}
	q.Order("id DESC").Find(&list)
	utils.OK(c, list)
}

func (r *Router) CreateCabinet(c *gin.Context) {
	var req CreateCabinetReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	if req.TotalSlots == 0 {
		req.TotalSlots = 12
	}
	tx := database.DB.Begin()
	cab := models.Cabinet{
		CabinetNo:  req.CabinetNo,
		Name:       req.Name,
		Address:    req.Address,
		Longitude:  req.Longitude,
		Latitude:   req.Latitude,
		TotalSlots: req.TotalSlots,
		Status:     models.CabinetOnline,
	}
	if err := tx.Create(&cab).Error; err != nil {
		tx.Rollback()
		utils.Fail(c, 500, "创建失败")
		return
	}
	slots := make([]models.Slot, 0, req.TotalSlots)
	for i := 1; i <= req.TotalSlots; i++ {
		slots = append(slots, models.Slot{
			CabinetID: cab.ID,
			SlotNo:    i,
			Status:    models.SlotEmpty,
		})
	}
	if len(slots) > 0 {
		if err := tx.Create(&slots).Error; err != nil {
			tx.Rollback()
			utils.Fail(c, 500, "创建格口失败")
			return
		}
	}
	tx.Commit()
	utils.OK(c, cab)
}

func (r *Router) GetCabinet(c *gin.Context) {
	id := utils.ParseUint(c.Param("id"))
	var cab models.Cabinet
	if err := database.DB.First(&cab, id).Error; err != nil {
		utils.Fail(c, 404, "不存在")
		return
	}
	var slots []models.Slot
	database.DB.Where("cabinet_id = ?", id).Preload("Battery").Order("slot_no ASC").Find(&slots)
	utils.OK(c, gin.H{"cabinet": cab, "slots": slots})
}

func (r *Router) UpdateCabinet(c *gin.Context) {
	id := utils.ParseUint(c.Param("id"))
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	database.DB.Model(&models.Cabinet{}).Where("id = ?", id).Updates(req)
	utils.OK(c, nil)
}

func (r *Router) ListBatteries(c *gin.Context) {
	var list []models.Battery
	q := database.DB.Model(&models.Battery{})
	if kw := c.Query("keyword"); kw != "" {
		q = q.Where("battery_no LIKE ? OR model LIKE ?", "%"+kw+"%", "%"+kw+"%")
	}
	if st := c.Query("status"); st != "" {
		q = q.Where("status = ?", st)
	}
	q.Order("id DESC").Limit(200).Find(&list)
	utils.OK(c, list)
}

type CreateBatteryReq struct {
	BatteryNo string `json:"battery_no" binding:"required"`
	Model     string `json:"model"`
	Capacity  int    `json:"capacity"`
	SOC       int    `json:"soc"`
}

func (r *Router) CreateBattery(c *gin.Context) {
	var req CreateBatteryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	if req.Capacity == 0 {
		req.Capacity = 10000
	}
	bat := models.Battery{
		BatteryNo: req.BatteryNo,
		Model:     req.Model,
		Capacity:  req.Capacity,
		SOC:       req.SOC,
		Status:    models.BatteryInCabinet,
	}
	if err := database.DB.Create(&bat).Error; err != nil {
		utils.Fail(c, 500, "创建失败")
		return
	}
	utils.OK(c, bat)
}

type AssignBatteryReq struct {
	BatteryNo string `json:"battery_no" binding:"required"`
	CabinetNo string `json:"cabinet_no" binding:"required"`
	SlotNo    int    `json:"slot_no" binding:"required"`
}

func (r *Router) AssignBattery(c *gin.Context) {
	var req AssignBatteryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	tx := database.DB.Begin()
	var cab models.Cabinet
	if err := tx.Where("cabinet_no = ?", req.CabinetNo).First(&cab).Error; err != nil {
		tx.Rollback()
		utils.Fail(c, 404, "柜机不存在")
		return
	}
	var slot models.Slot
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("cabinet_id = ? AND slot_no = ?", cab.ID, req.SlotNo).
		First(&slot).Error; err != nil {
		tx.Rollback()
		utils.Fail(c, 404, "格口不存在")
		return
	}
	if slot.Status == models.SlotOccupied {
		tx.Rollback()
		utils.Fail(c, 400, "格口已占用")
		return
	}
	var bat models.Battery
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("battery_no = ?", req.BatteryNo).First(&bat).Error; err != nil {
		tx.Rollback()
		utils.Fail(c, 404, "电池不存在")
		return
	}
	slot.Status = models.SlotOccupied
	slot.BatteryID = &bat.ID
	bat.Status = models.BatteryInCabinet
	bat.SlotID = &slot.ID
	tx.Save(&slot)
	tx.Save(&bat)
	tx.Commit()
	utils.OK(c, nil)
}

func (r *Router) ListRules(c *gin.Context) {
	var list []models.BillingRule
	database.DB.Order("id DESC").Find(&list)
	utils.OK(c, list)
}

func (r *Router) CreateRule(c *gin.Context) {
	var rule models.BillingRule
	if err := c.ShouldBindJSON(&rule); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	if rule.IsDefault {
		database.DB.Model(&models.BillingRule{}).Where("is_default = ?", true).Update("is_default", false)
	}
	if err := database.DB.Create(&rule).Error; err != nil {
		utils.Fail(c, 500, "创建失败")
		return
	}
	utils.OK(c, rule)
}

func (r *Router) UpdateRule(c *gin.Context) {
	id := utils.ParseUint(c.Param("id"))
	var req map[string]interface{}
	c.ShouldBindJSON(&req)
	if isDefault, ok := req["is_default"].(bool); ok && isDefault {
		database.DB.Model(&models.BillingRule{}).Where("is_default = ? AND id != ?", true, id).Update("is_default", false)
	}
	database.DB.Model(&models.BillingRule{}).Where("id = ?", id).Updates(req)
	utils.OK(c, nil)
}

func (r *Router) AdminListOrders(c *gin.Context) {
	var list []models.RentalOrder
	var total int64
	q := database.DB.Model(&models.RentalOrder{})
	if st := c.Query("status"); st != "" {
		q = q.Where("status = ?", st)
	}
	if uid := c.Query("user_id"); uid != "" {
		q = q.Where("user_id = ?", utils.ParseUint(uid))
	}
	if on := c.Query("order_no"); on != "" {
		q = q.Where("order_no = ?", on)
	}
	page := utils.ParseInt(c.DefaultQuery("page", "1"))
	size := utils.ParseInt(c.DefaultQuery("size", "20"))
	q.Count(&total)
	q.Preload("Battery").Preload("Rule").Order("id DESC").
		Offset((page - 1) * size).Limit(size).Find(&list)
	utils.OK(c, gin.H{"list": list, "total": total})
}

func (r *Router) ManualReturn(c *gin.Context) {
	var req handlers.ManualReturnReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	req.OperatorID = middleware.GetUserID(c)
	resp, err := r.returnS.ManualReturn(c.Request.Context(), &req)
	if err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, resp)
}

func (r *Router) MarkLost(c *gin.Context) {
	var req handlers.HandleLostReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	req.Operator = middleware.GetUserID(c)
	if err := r.op.MarkLost(c.Request.Context(), &req); err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, nil)
}

func (r *Router) MarkDamage(c *gin.Context) {
	var req handlers.HandleDamageReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	req.Operator = middleware.GetUserID(c)
	if err := r.op.MarkDamage(c.Request.Context(), &req); err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, nil)
}

func (r *Router) AdminListDisputes(c *gin.Context) {
	var list []models.DisputeRecord
	var total int64
	q := database.DB.Model(&models.DisputeRecord{})
	if st := c.Query("status"); st != "" {
		q = q.Where("status = ?", st)
	}
	q.Count(&total)
	q.Order("id DESC").Limit(100).Find(&list)
	utils.OK(c, gin.H{"list": list, "total": total})
}

func (r *Router) HandleDispute(c *gin.Context) {
	var req handlers.HandleDisputeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	req.Operator = middleware.GetUserID(c)
	if err := r.op.HandleDispute(c.Request.Context(), &req); err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, nil)
}

func (r *Router) ListRepairs(c *gin.Context) {
	var list []models.RepairRecord
	q := database.DB.Model(&models.RepairRecord{})
	if st := c.Query("status"); st != "" {
		q = q.Where("status = ?", st)
	}
	q.Order("id DESC").Limit(200).Find(&list)
	utils.OK(c, list)
}

func (r *Router) CreateRepair(c *gin.Context) {
	var req handlers.CreateRepairReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	op := middleware.GetUserID(c)
	rec, err := r.op.CreateRepair(c.Request.Context(), op, &req)
	if err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, rec)
}

type UpdateRepairReq struct {
	Status  string `json:"status" binding:"required"`
	Cost    int64  `json:"cost"`
	Remarks string `json:"remarks"`
}

func (r *Router) UpdateRepair(c *gin.Context) {
	id := utils.ParseUint(c.Param("id"))
	var req UpdateRepairReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	op := middleware.GetUserID(c)
	if err := r.op.ResolveRepair(c.Request.Context(), id, op, req.Status, req.Cost, req.Remarks); err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, nil)
}

func (r *Router) ListExceptions(c *gin.Context) {
	var list []models.ExceptionRecord
	q := database.DB.Model(&models.ExceptionRecord{})
	if et := c.Query("type"); et != "" {
		q = q.Where("excep_type = ?", et)
	}
	q.Order("id DESC").Limit(200).Find(&list)
	utils.OK(c, list)
}

func (r *Router) ListReports(c *gin.Context) {
	var list []models.CabinetReport
	q := database.DB.Model(&models.CabinetReport{})
	if cn := c.Query("cabinet_no"); cn != "" {
		var cab models.Cabinet
		if err := database.DB.Where("cabinet_no = ?", cn).First(&cab).Error; err == nil {
			q = q.Where("cabinet_id = ?", cab.ID)
		}
	}
	if rt := c.Query("report_type"); rt != "" {
		q = q.Where("report_type = ?", rt)
	}
	q.Order("id DESC").Limit(200).Find(&list)
	utils.OK(c, list)
}

type BatchCreateSlotsReq struct {
	CabinetID uint64 `json:"cabinet_id" binding:"required"`
	StartNo   int    `json:"start_no"`
	Count     int    `json:"count" binding:"required"`
}

func (r *Router) BatchCreateSlots(c *gin.Context) {
	var req BatchCreateSlotsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	if req.StartNo == 0 {
		req.StartNo = 1
	}
	slots := make([]models.Slot, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		slots = append(slots, models.Slot{
			CabinetID: req.CabinetID,
			SlotNo:    req.StartNo + i,
			Status:    models.SlotEmpty,
		})
	}
	if err := database.DB.Create(&slots).Error; err != nil {
		utils.Fail(c, 500, "批量创建失败")
		return
	}
	utils.OK(c, slots)
}

func (r *Router) ReportUnlockFailure(c *gin.Context) {
	var req handlers.ReportUnlockFailureReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误："+err.Error())
		return
	}
	uid := middleware.GetUserID(c)
	resp, err := r.rental.ReportUnlockFailure(c.Request.Context(), uid, &req)
	if err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}
	utils.OK(c, resp)
}

func (r *Router) ListReturnRecords(c *gin.Context) {
	var list []models.ReturnRecord
	var total int64
	q := database.DB.Model(&models.ReturnRecord{})
	if cc := c.Query("cross_category"); cc != "" {
		q = q.Where("cross_category = ?", cc)
	}
	if cn := c.Query("cabinet_no"); cn != "" {
		var cab models.Cabinet
		if err := database.DB.Where("cabinet_no = ?", cn).First(&cab).Error; err == nil {
			q = q.Where("cabinet_id = ?", cab.ID)
		}
	}
	q.Count(&total)
	page := utils.ParseInt(c.DefaultQuery("page", "1"))
	size := utils.ParseInt(c.DefaultQuery("size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 200 {
		size = 20
	}
	err := q.Preload("Order").Preload("Order.Battery").
		Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&list).Error
	if err != nil {
		utils.Fail(c, 500, "查询失败")
		return
	}
	utils.OK(c, gin.H{"list": list, "total": total, "page": page, "size": size})
}

func (r *Router) GetReturnRecordDetail(c *gin.Context) {
	id := utils.ParseUint(c.Param("id"))
	var rec models.ReturnRecord
	if err := database.DB.Preload("Order").Preload("Order.Battery").Preload("Order.Rule").
		First(&rec, id).Error; err != nil {
		utils.Fail(c, 404, "记录不存在")
		return
	}
	var cab models.Cabinet
	var slot models.Slot
	var battery models.Battery
	database.DB.First(&cab, rec.CabinetID)
	database.DB.First(&slot, rec.SlotID)
	database.DB.First(&battery, rec.BatteryID)
	utils.OK(c, gin.H{
		"record":   rec,
		"cabinet":  cab,
		"slot":     slot,
		"battery":  battery,
	})
}

type HandleCompensationReq struct {
	OrderNo  string `json:"order_no" binding:"required"`
	Action   string `json:"action" binding:"required"`
	Operator uint64 `json:"-"`
	Remarks  string `json:"remarks"`
}

func (r *Router) HandleAbnormalCompensation(c *gin.Context) {
	var req HandleCompensationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "参数错误")
		return
	}
	req.Operator = middleware.GetUserID(c)

	var order models.RentalOrder
	if err := database.DB.Where("order_no = ?", req.OrderNo).First(&order).Error; err != nil {
		utils.Fail(c, 404, "订单不存在")
		return
	}
	if order.Status != models.OrderAbnormal {
		utils.Fail(c, 400, "订单状态不是异常单，无需补偿")
		return
	}
	if order.CompensationStatus != 0 && order.CompensationStatus != 2 {
		utils.Fail(c, 400, "该订单已处理过补偿")
		return
	}

	tx := database.DB.Begin()
	now := time.Now()
	action := models.CompensationAction(req.Action)

	switch action {
	case models.CompensationRefundDeposit:
		if order.DepositStatus == 1 && order.DepositAmt > 0 {
			var user models.User
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, order.UserID).Error; err != nil {
				tx.Rollback()
				utils.Fail(c, 500, "查询用户失败")
				return
			}
			refundAmt := order.DepositAmt
			user.Balance += refundAmt
			if err := tx.Save(&user).Error; err != nil {
				tx.Rollback()
				utils.Fail(c, 500, "更新用户余额失败")
				return
			}
			tx.Create(&models.DepositRecord{
				OrderID:     order.ID,
				UserID:      order.UserID,
				Action:      models.DepositRelease,
				Amount:      refundAmt,
				BeforeBal:   user.Balance - refundAmt,
				AfterBal:    user.Balance,
				TxnID:       utils.GenTxnID(),
				Status:      1,
				Reason:      fmt.Sprintf("运营开柜失败补偿押金退还：订单%s，备注：%s", order.OrderNo, req.Remarks),
				ProcessedAt: &now,
			})
			order.DepositStatus = 2
			order.RefundAmt = refundAmt
		}
		order.Status = models.OrderAbnormalCompensated
		order.CompensationStatus = 1
	case models.CompensationRecreateOrder:
		if order.BatteryID == 0 {
			tx.Rollback()
			utils.Fail(c, 400, "订单无电池信息，无法补开")
			return
		}
		order.Status = models.OrderRenting
		order.BillingEnabled = true
		order.StartTime = &now
		order.CompensationStatus = 1
	case models.CompensationManualReview:
		order.CompensationStatus = 2
	default:
		tx.Rollback()
		utils.Fail(c, 400, "不支持的补偿动作："+req.Action)
		return
	}

	updates := map[string]interface{}{
		"status":              order.Status,
		"billing_enabled":     order.BillingEnabled,
		"compensation_action": action,
		"compensation_status": order.CompensationStatus,
		"deposit_status":      order.DepositStatus,
		"refund_amt":          order.RefundAmt,
		"compensation_remark": fmt.Sprintf("[运营处理]%s | 原备注：%s", req.Remarks, order.CompensationRemark),
		"compensated_by":      &req.Operator,
		"compensated_at":      &now,
	}
	if order.StartTime != nil && action == models.CompensationRecreateOrder {
		updates["start_time"] = order.StartTime
	}

	if err := tx.Model(&order).Updates(updates).Error; err != nil {
		tx.Rollback()
		utils.Fail(c, 500, "更新订单失败")
		return
	}

	tx.Model(&models.ExceptionRecord{}).Where("order_id = ?", order.ID).
		Updates(map[string]interface{}{
			"status":     1,
			"handled_by": &req.Operator,
			"handled_at": &now,
		})

	if err := tx.Commit().Error; err != nil {
		utils.Fail(c, 500, "提交事务失败")
		return
	}
	utils.OK(c, gin.H{
		"order_no":           order.OrderNo,
		"action":             action,
		"compensation_status": order.CompensationStatus,
	})
}

func (r *Router) ListAbnormalOrders(c *gin.Context) {
	var list []models.RentalOrder
	var total int64
	q := database.DB.Model(&models.RentalOrder{}).Where("status IN ?", []models.OrderStatus{
		models.OrderAbnormal, models.OrderAbnormalCompensated,
	})
	if ar := c.Query("abnormal_reason"); ar != "" {
		q = q.Where("abnormal_reason = ?", ar)
	}
	if cs := c.Query("compensation_status"); cs != "" {
		q = q.Where("compensation_status = ?", utils.ParseInt(cs))
	}
	q.Count(&total)
	page := utils.ParseInt(c.DefaultQuery("page", "1"))
	size := utils.ParseInt(c.DefaultQuery("size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	q.Preload("Battery").Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&list)
	utils.OK(c, gin.H{"list": list, "total": total, "page": page, "size": size})
}

func (r *Router) GetAbnormalOrderDetail(c *gin.Context) {
	id := utils.ParseUint(c.Param("id"))
	var order models.RentalOrder
	if err := database.DB.Preload("Battery").Preload("Rule").
		Where("id = ? AND status IN ?", id, []models.OrderStatus{
			models.OrderAbnormal, models.OrderAbnormalCompensated,
		}).First(&order).Error; err != nil {
		utils.Fail(c, 404, "异常订单不存在")
		return
	}
	var exceptions []models.ExceptionRecord
	database.DB.Where("order_id = ?", id).Find(&exceptions)
	var depos []models.DepositRecord
	database.DB.Where("order_id = ?", id).Find(&depos)
	var fromCabinet models.Cabinet
	database.DB.First(&fromCabinet, order.FromCabinetID)
	var fromSlot models.Slot
	database.DB.First(&fromSlot, order.FromSlotID)
	evidence := gin.H{
		"abnormal_reason":      order.AbnormalReason,
		"compensation_action":  order.CompensationAction,
		"compensation_status":  order.CompensationStatus,
		"compensation_remark":  order.CompensationRemark,
		"billing_enabled":      order.BillingEnabled,
		"deposit_amt":          order.DepositAmt,
		"deposit_status":       order.DepositStatus,
		"refund_amt":           order.RefundAmt,
		"from_cabinet":         fromCabinet,
		"from_slot":            fromSlot,
	}
	if order.CompensatedBy != nil {
		var operator models.User
		if database.DB.First(&operator, *order.CompensatedBy).Error == nil {
			evidence["compensated_by"] = gin.H{"id": operator.ID, "phone": operator.Phone, "role": operator.Role}
		}
	}
	if order.CompensatedAt != nil {
		evidence["compensated_at"] = order.CompensatedAt
	}
	utils.OK(c, gin.H{
		"order":      order,
		"exceptions": exceptions,
		"deposits":   depos,
		"evidence":   evidence,
	})
}
