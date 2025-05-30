package config

import (
	"fmt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"os"
)

var database *gorm.DB
var e error

var SALT = getEnv("SALT")

const AccessTokenValidality = 14

func DatabaseInit() {

	host := getEnv("DB_HOST")
	port := getEnv("DB_PORT")
	user := getEnv("DB_USER")
	password := getEnv("DB_PASSWORD")
	dbName := getEnv("DB_NAME")

	dsn := fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%s sslmode=disable TimeZone=Europe/Moscow", host, user, password, dbName, port)
	database, e = gorm.Open(postgres.Open(dsn), &gorm.Config{})

	if e != nil {
		panic(e)
	}
}

func DB() *gorm.DB {
	return database
}

func AutoMigrate() error {
	errUser := DB().AutoMigrate(&User{})
	if errUser != nil {
		return errUser
	}

	errToken := DB().AutoMigrate(&Token{})
	if errToken != nil {
		return errToken
	}

	errCompany := DB().AutoMigrate(&Company{})
	if errCompany != nil {
		return errCompany
	}

	errDecree := DB().AutoMigrate(&Decree{})
	if errDecree != nil {
		return errDecree
	}

	errGrant := DB().AutoMigrate(&Grant{})
	if errGrant != nil {
		return errGrant
	}

	errSample := DB().AutoMigrate(&Sample{})
	if errSample != nil {
		return errSample
	}

	errUserSample := DB().AutoMigrate(&UserSample{})
	if errUserSample != nil {
		return errUserSample
	}

	errBlockedOkveds := DB().AutoMigrate(&BlockedOkveds{})
	if errBlockedOkveds != nil {
		return errBlockedOkveds
	}

	errOkveds := DB().AutoMigrate(&Okveds{})
	if errOkveds != nil {
		return errOkveds
	}

	errActiveSubscription := DB().AutoMigrate(&ActiveSubscription{})
	if errActiveSubscription != nil {
		return errActiveSubscription
	}

	errLogs := DB().AutoMigrate(&Log{})
	if errLogs != nil {
		return errLogs
	}

	InitOkveds()
	InitBlockedOkveds()

	return nil
}

func getEnv(key string) string {
	value, exists := os.LookupEnv(key)
	if exists {
		return value
	} else {
		panic(fmt.Sprintf("Environment variable %s not set", key))
	}
	return "nil"
}
