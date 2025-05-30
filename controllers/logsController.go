package controllers

import (
	"github.com/jackc/pgx/v5/pgtype"
	"park/config"
	"time"
)

func AddLog(userId int, action string, description string) {
	db := config.DB()

	log := config.Log{
		UserID:      userId,
		Action:      action,
		Description: description,
		DateTime:    pgtype.Date{Time: time.Now(), Valid: true},
	}

	db.Create(&log)
}
