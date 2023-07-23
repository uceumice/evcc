package config

import (
	"fmt"

	"gorm.io/gorm"
)

type Config struct {
	ID      int `gorm:"primarykey"`
	Class   Class
	Type    string
	Details []ConfigDetail `gorm:"constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

type ConfigDetail struct {
	ConfigID int    `gorm:"index:idx_unique"`
	Key      string `gorm:"index:idx_unique"`
	Value    string
}

// Named converts device details to named config
func (d *Config) Named() Named {
	res := Named{
		Name:  NameForID(d.ID),
		Type:  d.Type,
		Other: d.detailsAsMap(),
	}
	return res
}

// Typed converts device details to typed config
func (d *Config) Typed() Typed {
	res := Typed{
		Type:  d.Type,
		Other: d.detailsAsMap(),
	}
	return res
}

// detailsAsMap converts device details to map
func (d *Config) detailsAsMap() map[string]any {
	res := make(map[string]any, len(d.Details))
	for _, detail := range d.Details {
		res[detail.Key] = detail.Value
	}
	return res
}

// mapAsDetails converts map to device details
func (d *Config) mapAsDetails(config map[string]any) []ConfigDetail {
	res := make([]ConfigDetail, 0, len(config))
	for k, v := range config {
		res = append(res, ConfigDetail{ConfigID: d.ID, Key: k, Value: fmt.Sprintf("%v", v)})
	}
	return res
}

var db *gorm.DB

func Init(instance *gorm.DB) error {
	db = instance
	return db.AutoMigrate(new(Config), new(ConfigDetail))
}

// NameForID returns a unique config name for the given id
func NameForID(id int) string {
	return fmt.Sprintf("db:%d", id)
}

// ConfigurationsByClass returns devices by class from the database
func ConfigurationsByClass(class Class) ([]Config, error) {
	var devices []Config
	tx := db.Where(&Config{Class: class}).Preload("Details").Order("id").Find(&devices)

	// remove devices without details
	for i := 0; i < len(devices); {
		if len(devices[i].Details) > 0 {
			i++
			continue
		}

		// delete device
		copy(devices[i:], devices[i+1:])
		devices = devices[: len(devices)-1 : len(devices)-1]
	}

	return devices, tx.Error
}

// ConfigByID returns device by id from the database
func ConfigByID(id int) (Config, error) {
	var device Config
	tx := db.Where(&Config{ID: id}).Preload("Details").First(&device)
	return device, tx.Error
}

// AddConfig adds a new device to the database
func AddConfig(class Class, typ string, config map[string]any) (int, error) {
	device := Config{Class: class, Type: typ}

	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&device).Error; err != nil {
			return err
		}

		details := device.mapAsDetails(config)
		return tx.Create(&details).Error
	})

	return device.ID, err
}

// UpdateConfig updates a device's details to the database
func UpdateConfig(class Class, id int, config map[string]any) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var device Config
		if err := tx.Where(Config{Class: class, ID: id}).First(&device).Error; err != nil {
			return err
		}

		if err := tx.Delete(new(ConfigDetail), ConfigDetail{ConfigID: id}).Error; err != nil {
			return err
		}

		details := device.mapAsDetails(config)
		return tx.Save(&details).Error
	})
}

// DeleteConfig deletes a device from the database
func DeleteConfig(class Class, id int) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(new(ConfigDetail), ConfigDetail{ConfigID: id}).Error; err != nil {
			return err
		}

		return tx.Delete(Config{ID: id}).Error
	})
}
