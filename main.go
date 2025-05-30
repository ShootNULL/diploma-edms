package main

import (
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"net/http"
	"park/config"
	"park/controllers"
)

const keyValidality = 14

func main() {
	e := echo.New()

	config.DatabaseInit()
	gorm := config.DB()

	dbGorm, err := gorm.DB()
	if err != nil {
		panic(err)
	}

	dbGorm.Ping()

	err = config.AutoMigrate()

	if err != nil {
		panic(err)
	}

	config.InitMinio()

	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins:     []string{"https://fintechnik.online"},
		AllowMethods:     []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowHeaders:     []string{"*"},
		AllowCredentials: true,
	}))

	controllers.AddRoutes(e)

	e.Logger.Fatal(e.Start(":8080"))

}
