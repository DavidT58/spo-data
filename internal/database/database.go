package database

import (
	// "database/sql"
	"log"
	"os"
	"time"

	"spo-data/internal/models"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	// _ "github.com/mattn/go-sqlite3" // SQLite driver
	"github.com/glebarez/sqlite"
)

var db *gorm.DB

// Initialize opens a database connection and sets up the schema
func Initialize(dbPath string) error {
	var err error
	// Quiet logger: surface real warnings/errors but ignore the expected
	// "record not found" misses (every healthy pool's alert-state lookup is a
	// miss), which would otherwise flood the daemon's logs each cycle.
	dbLogger := gormlogger.New(
		log.New(os.Stdout, "", log.LstdFlags),
		gormlogger.Config{
			SlowThreshold:             2 * time.Second,
			LogLevel:                  gormlogger.Warn,
			IgnoreRecordNotFoundError: true,
		},
	)
	db, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: dbLogger})
	if err != nil {
		return err
	}

	db.AutoMigrate(&models.Price{}, &models.PoolAlertState{}, &models.PoolKesState{})

	return nil

}

// GetAlertState returns the stored alert state for a (pool, alertType) pair.
// found is false when no row exists yet (a fresh pool/alert type).
func GetAlertState(poolID, alertType string) (state models.PoolAlertState, found bool, err error) {
	result := db.Where("pool_id = ? AND alert_type = ?", poolID, alertType).First(&state)
	if result.Error == gorm.ErrRecordNotFound {
		return models.PoolAlertState{}, false, nil
	}
	if result.Error != nil {
		return models.PoolAlertState{}, false, result.Error
	}
	return state, true, nil
}

// SaveAlertState inserts or updates an alert state row.
func SaveAlertState(state *models.PoolAlertState) error {
	return db.Save(state).Error
}

// GetKesState returns the stored KES tracking state for a pool. found is false
// when the pool has not been observed yet (triggers the initial back-scan).
func GetKesState(poolID string) (state models.PoolKesState, found bool, err error) {
	result := db.Where("pool_id = ?", poolID).First(&state)
	if result.Error == gorm.ErrRecordNotFound {
		return models.PoolKesState{}, false, nil
	}
	if result.Error != nil {
		return models.PoolKesState{}, false, result.Error
	}
	return state, true, nil
}

// SaveKesState inserts or updates a KES tracking row.
func SaveKesState(state *models.PoolKesState) error {
	return db.Save(state).Error
}

// StoreValue inserts a value into the database
func StorePrice(price models.Price) (models.Price, error) {
	result := db.Create(&price)

	if result.Error != nil {
		return models.Price{}, result.Error
	}

	return price, nil
}

// GetValueByName retrieves a value from the database by its name
func GetLastPrice() (models.Price, error) {
	var price models.Price
	result := db.Last(&price)
	if result.Error != nil {
		return models.Price{}, result.Error
	}

	return price, nil
}
