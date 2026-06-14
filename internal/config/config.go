package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	ServerPort   string
	ServerMode   string
	PostgresHost string
	PostgresPort string
	PostgresUser string
	PostgresPass string
	PostgresDB   string
	PostgresSSL  string
	RedisHost    string
	RedisPort    string
	RedisPass    string
	RedisDB      int
	JWTSecret    string
	JWTExpire    int
	DepositAmt   int64
}

var AppConfig *Config

func Load() error {
	_ = godotenv.Load()

	redisDB, _ := strconv.Atoi(getEnv("REDIS_DB", "0"))
	jwtExpire, _ := strconv.Atoi(getEnv("JWT_EXPIRE_HOURS", "72"))
	depositAmt, _ := strconv.ParseInt(getEnv("DEPOSIT_AMOUNT", "29900"), 10, 64)

	AppConfig = &Config{
		ServerPort:   getEnv("SERVER_PORT", "8080"),
		ServerMode:   getEnv("SERVER_MODE", "debug"),
		PostgresHost: getEnv("POSTGRES_HOST", "localhost"),
		PostgresPort: getEnv("POSTGRES_PORT", "5432"),
		PostgresUser: getEnv("POSTGRES_USER", "postgres"),
		PostgresPass: getEnv("POSTGRES_PASSWORD", "postgres"),
		PostgresDB:   getEnv("POSTGRES_DB", "battery_rental"),
		PostgresSSL:  getEnv("POSTGRES_SSLMODE", "disable"),
		RedisHost:    getEnv("REDIS_HOST", "localhost"),
		RedisPort:    getEnv("REDIS_PORT", "6379"),
		RedisPass:    getEnv("REDIS_PASSWORD", ""),
		RedisDB:      redisDB,
		JWTSecret:    getEnv("JWT_SECRET", "default-secret-change-me"),
		JWTExpire:    jwtExpire,
		DepositAmt:   depositAmt,
	}
	return nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
