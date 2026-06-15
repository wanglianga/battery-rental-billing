package database

import (
	"fmt"
	"log"
	"time"

	"battery-rental/internal/config"
	"battery-rental/internal/models"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

func Connect() error {
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s TimeZone=Asia/Shanghai",
		config.AppConfig.PostgresHost,
		config.AppConfig.PostgresPort,
		config.AppConfig.PostgresUser,
		config.AppConfig.PostgresPass,
		config.AppConfig.PostgresDB,
		config.AppConfig.PostgresSSL,
	)

	var err error
	DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}

	sqlDB, err := DB.DB()
	if err != nil {
		return fmt.Errorf("get sql db: %w", err)
	}
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetConnMaxLifetime(time.Hour)

	log.Println("[DB] connected to postgres")
	return nil
}

func Migrate() error {
	err := DB.AutoMigrate(
		&models.User{},
		&models.Cabinet{},
		&models.Slot{},
		&models.Battery{},
		&models.BillingRule{},
		&models.RentalOrder{},
		&models.DepositRecord{},
		&models.ReturnRecord{},
		&models.ExceptionRecord{},
		&models.RepairRecord{},
		&models.CabinetReport{},
		&models.DisputeRecord{},
		&models.PaymentRecord{},
		&models.IdempotentRecord{},
	)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	if err := addForeignKeys(); err != nil {
		return fmt.Errorf("add foreign keys: %w", err)
	}

	if err := createIndexes(); err != nil {
		return fmt.Errorf("create indexes: %w", err)
	}

	log.Println("[DB] migration completed")
	return nil
}

func addForeignKeys() error {
	type fkDef struct {
		table      string
		constraint string
		sql        string
	}
	statements := []fkDef{
		{"slots", "fk_slots_cabinet", "ALTER TABLE slots ADD CONSTRAINT fk_slots_cabinet FOREIGN KEY (cabinet_id) REFERENCES cabinets(id) ON DELETE CASCADE"},
		{"slots", "fk_slots_battery", "ALTER TABLE slots ADD CONSTRAINT fk_slots_battery FOREIGN KEY (battery_id) REFERENCES batteries(id) ON DELETE SET NULL"},
		{"batteries", "fk_batteries_slot", "ALTER TABLE batteries ADD CONSTRAINT fk_batteries_slot FOREIGN KEY (slot_id) REFERENCES slots(id) ON DELETE SET NULL"},
		{"rental_orders", "fk_orders_user", "ALTER TABLE rental_orders ADD CONSTRAINT fk_orders_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE RESTRICT"},
		{"rental_orders", "fk_orders_battery", "ALTER TABLE rental_orders ADD CONSTRAINT fk_orders_battery FOREIGN KEY (battery_id) REFERENCES batteries(id) ON DELETE RESTRICT"},
		{"rental_orders", "fk_orders_from_cabinet", "ALTER TABLE rental_orders ADD CONSTRAINT fk_orders_from_cabinet FOREIGN KEY (from_cabinet_id) REFERENCES cabinets(id) ON DELETE RESTRICT"},
		{"rental_orders", "fk_orders_from_slot", "ALTER TABLE rental_orders ADD CONSTRAINT fk_orders_from_slot FOREIGN KEY (from_slot_id) REFERENCES slots(id) ON DELETE RESTRICT"},
		{"rental_orders", "fk_orders_to_cabinet", "ALTER TABLE rental_orders ADD CONSTRAINT fk_orders_to_cabinet FOREIGN KEY (to_cabinet_id) REFERENCES cabinets(id) ON DELETE SET NULL"},
		{"rental_orders", "fk_orders_to_slot", "ALTER TABLE rental_orders ADD CONSTRAINT fk_orders_to_slot FOREIGN KEY (to_slot_id) REFERENCES slots(id) ON DELETE SET NULL"},
		{"rental_orders", "fk_orders_rule", "ALTER TABLE rental_orders ADD CONSTRAINT fk_orders_rule FOREIGN KEY (rule_id) REFERENCES billing_rules(id) ON DELETE RESTRICT"},
		{"deposit_records", "fk_deposits_order", "ALTER TABLE deposit_records ADD CONSTRAINT fk_deposits_order FOREIGN KEY (order_id) REFERENCES rental_orders(id) ON DELETE CASCADE"},
		{"deposit_records", "fk_deposits_user", "ALTER TABLE deposit_records ADD CONSTRAINT fk_deposits_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE"},
		{"return_records", "fk_returns_order", "ALTER TABLE return_records ADD CONSTRAINT fk_returns_order FOREIGN KEY (order_id) REFERENCES rental_orders(id) ON DELETE CASCADE"},
		{"return_records", "fk_returns_cabinet", "ALTER TABLE return_records ADD CONSTRAINT fk_returns_cabinet FOREIGN KEY (cabinet_id) REFERENCES cabinets(id) ON DELETE RESTRICT"},
		{"return_records", "fk_returns_slot", "ALTER TABLE return_records ADD CONSTRAINT fk_returns_slot FOREIGN KEY (slot_id) REFERENCES slots(id) ON DELETE RESTRICT"},
		{"return_records", "fk_returns_battery", "ALTER TABLE return_records ADD CONSTRAINT fk_returns_battery FOREIGN KEY (battery_id) REFERENCES batteries(id) ON DELETE RESTRICT"},
		{"return_records", "fk_returns_user", "ALTER TABLE return_records ADD CONSTRAINT fk_returns_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE RESTRICT"},
		{"exception_records", "fk_exceptions_order", "ALTER TABLE exception_records ADD CONSTRAINT fk_exceptions_order FOREIGN KEY (order_id) REFERENCES rental_orders(id) ON DELETE SET NULL"},
		{"exception_records", "fk_exceptions_user", "ALTER TABLE exception_records ADD CONSTRAINT fk_exceptions_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE RESTRICT"},
		{"exception_records", "fk_exceptions_battery", "ALTER TABLE exception_records ADD CONSTRAINT fk_exceptions_battery FOREIGN KEY (battery_id) REFERENCES batteries(id) ON DELETE SET NULL"},
		{"exception_records", "fk_exceptions_handler", "ALTER TABLE exception_records ADD CONSTRAINT fk_exceptions_handler FOREIGN KEY (handled_by) REFERENCES users(id) ON DELETE SET NULL"},
		{"repair_records", "fk_repairs_reporter", "ALTER TABLE repair_records ADD CONSTRAINT fk_repairs_reporter FOREIGN KEY (report_by) REFERENCES users(id) ON DELETE SET NULL"},
		{"repair_records", "fk_repairs_repairer", "ALTER TABLE repair_records ADD CONSTRAINT fk_repairs_repairer FOREIGN KEY (repair_by) REFERENCES users(id) ON DELETE SET NULL"},
		{"cabinet_reports", "fk_reports_cabinet", "ALTER TABLE cabinet_reports ADD CONSTRAINT fk_reports_cabinet FOREIGN KEY (cabinet_id) REFERENCES cabinets(id) ON DELETE CASCADE"},
		{"dispute_records", "fk_disputes_order", "ALTER TABLE dispute_records ADD CONSTRAINT fk_disputes_order FOREIGN KEY (order_id) REFERENCES rental_orders(id) ON DELETE CASCADE"},
		{"dispute_records", "fk_disputes_user", "ALTER TABLE dispute_records ADD CONSTRAINT fk_disputes_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE"},
		{"dispute_records", "fk_disputes_handler", "ALTER TABLE dispute_records ADD CONSTRAINT fk_disputes_handler FOREIGN KEY (handled_by) REFERENCES users(id) ON DELETE SET NULL"},
		{"payment_records", "fk_payments_order", "ALTER TABLE payment_records ADD CONSTRAINT fk_payments_order FOREIGN KEY (order_id) REFERENCES rental_orders(id) ON DELETE SET NULL"},
		{"payment_records", "fk_payments_user", "ALTER TABLE payment_records ADD CONSTRAINT fk_payments_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE"},
	}

	for _, fk := range statements {
		var exists int
		err := DB.Raw(`
			SELECT 1 FROM information_schema.table_constraints 
			WHERE constraint_name = ? AND table_name = ? AND constraint_type = 'FOREIGN KEY'
		`, fk.constraint, fk.table).Scan(&exists).Error
		if err == nil && exists == 1 {
			continue
		}
		if err := DB.Exec(fk.sql).Error; err != nil {
			log.Printf("[DB] warn adding FK %s: %v", fk.constraint, err)
		} else {
			log.Printf("[DB] added FK: %s", fk.constraint)
		}
	}
	return nil
}

func createIndexes() error {
	statements := []string{
		"CREATE INDEX IF NOT EXISTS idx_orders_user_status ON rental_orders(user_id, status)",
		"CREATE INDEX IF NOT EXISTS idx_orders_battery_status ON rental_orders(battery_id, status)",
		"CREATE INDEX IF NOT EXISTS idx_orders_created_desc ON rental_orders(created_at DESC)",
		"CREATE INDEX IF NOT EXISTS idx_reports_cabinet_seq ON cabinet_reports(cabinet_id, report_seq, report_type)",
		"CREATE INDEX IF NOT EXISTS idx_reports_processed ON cabinet_reports(processed, report_type)",
		"CREATE INDEX IF NOT EXISTS idx_slots_cabinet_status ON slots(cabinet_id, status)",
		"CREATE INDEX IF NOT EXISTS idx_batteries_status ON batteries(status)",
		"CREATE INDEX IF NOT EXISTS idx_disputes_status ON dispute_records(status)",
		"CREATE INDEX IF NOT EXISTS idx_repairs_status ON repair_records(status)",
		"CREATE INDEX IF NOT EXISTS idx_exceptions_type_status ON exception_records(excep_type, status)",
		"CREATE INDEX IF NOT EXISTS idx_deposits_user_action ON deposit_records(user_id, action, created_at DESC)",
		"CREATE INDEX IF NOT EXISTS idx_payments_status ON payment_records(status)",
		"CREATE INDEX IF NOT EXISTS idx_payments_third_txn ON payment_records(third_txn_no)",
		"CREATE INDEX IF NOT EXISTS idx_idempotent_keys ON idempotent_records(key, expire_at)",
	}
	for _, stmt := range statements {
		if err := DB.Exec(stmt).Error; err != nil {
			log.Printf("[DB] warn adding index: %v", err)
		}
	}
	return nil
}

func Seed() error {
	var ruleCount int64
	DB.Model(&models.BillingRule{}).Count(&ruleCount)
	if ruleCount == 0 {
		defaultRule := models.BillingRule{
			Name:           "标准计费规则",
			FirstFreeMin:   5,
			FirstPeriodMin: 30,
			FirstPrice:     200,
			UnitMin:        30,
			UnitPrice:      100,
			DailyCap:       3000,
			MaxDays:        30,
			MaxFee:         29900,
			LostFee:        29900,
			DamageFee:      9900,
			IsDefault:      true,
			Active:         true,
		}
		if err := DB.Create(&defaultRule).Error; err != nil {
			return fmt.Errorf("seed billing rule: %w", err)
		}
		log.Println("[DB] seeded default billing rule")
	}

	var adminCount int64
	DB.Model(&models.User{}).Where("role = ?", models.RoleAdmin).Count(&adminCount)
	if adminCount == 0 {
		passwordHash, err := bcrypt.GenerateFromPassword([]byte("admin123"), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash admin password: %w", err)
		}
		admin := models.User{
			Phone:        "13800000000",
			Nickname:     "系统管理员",
			PasswordHash: string(passwordHash),
			Role:         models.RoleAdmin,
			Balance:      0,
			DepositFree:  true,
			Status:       1,
		}
		if err := DB.Create(&admin).Error; err != nil {
			return fmt.Errorf("seed admin: %w", err)
		}
		log.Println("[DB] seeded default admin user")
	}

	return nil
}
