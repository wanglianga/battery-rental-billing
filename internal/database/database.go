package database

import (
	"fmt"
	"log"
	"time"

	"battery-rental/internal/config"
	"battery-rental/internal/models"

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
	log.Println("[DB] migration completed")
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
		admin := models.User{
			Phone:        "13800000000",
			Nickname:     "系统管理员",
			PasswordHash: "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
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
