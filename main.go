// config.yaml (рядом с main.go)
//
// server:
//   port: "8080"
//   success_url: "https://example.com/success"
//   fail_url: "https://example.com/fail"
//
// tinkoff:
//   terminal_key: "YOUR_TERMINAL_KEY"
//   secret_key: "YOUR_SECRET_KEY"
//   notification_url: "https://yourdomain.com/notify"
//
// database:
//   dialect: "sqlite3"
//   connection: "payments.db"
//
// cors:
//   allowed_origins:
//     - "*"
//
// rcon:
//   address: "127.0.0.1:25575"
//   password: "minecraft_rcon_password"
//   command: "op {player}"

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorcon/rcon"
	"github.com/nikita-vanyasin/tinkoff"
	"github.com/rs/cors"
	"github.com/spf13/viper"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Config описывает конфигурацию сервиса.
type Config struct {
	Server struct {
		Port       string `mapstructure:"port"`
		SuccessURL string `mapstructure:"success_url"`
		FailURL    string `mapstructure:"fail_url"`
	} `mapstructure:"server"`
	Tinkoff struct {
		TerminalKey     string `mapstructure:"terminal_key"`
		SecretKey       string `mapstructure:"secret_key"`
		NotificationURL string `mapstructure:"notification_url"`
	} `mapstructure:"tinkoff"`
	Database struct {
		Dialect    string `mapstructure:"dialect"`
		Connection string `mapstructure:"connection"`
	} `mapstructure:"database"`
	CORS struct {
		AllowedOrigins []string `mapstructure:"allowed_origins"`
	} `mapstructure:"cors"`
	RCON struct {
		Address  string `mapstructure:"address"`
		Password string `mapstructure:"password"`
		Command  string `mapstructure:"command"`
	} `mapstructure:"rcon"`
}

var (
	config        Config
	db            *gorm.DB
	tinkoffClient *tinkoff.Client
)

// Payment хранит данные о платеже.
type Payment struct {
	gorm.Model
	OrderID    string `gorm:"uniqueIndex"`
	PaymentID  string
	Amount     uint64
	PaymentURL string
	Status     string
	PlayerName string
}

// PaymentRequest для инициализации платежа.
type PaymentRequest struct {
	OrderID    string `json:"order_id"`
	Amount     uint64 `json:"amount"`
	ClientIP   string `json:"client_ip"`
	PlayerName string `json:"player_name"`
}

// PaymentResponse возвращает URL для оплаты.
type PaymentResponse struct {
	PaymentURL string `json:"payment_url"`
}

// Notification – структура уведомления от Tinkoff Acquiring.
type Notification struct {
	OrderID   string      `json:"OrderId"`
	PaymentID json.Number `json:"PaymentId"`
	Status    string      `json:"Status"`
}

// loadConfig загружает настройки из config.yaml.
func loadConfig() error {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	_ = viper.ReadInConfig()
	viper.SetDefault("server.port", "8080")
	viper.SetDefault("database.dialect", "sqlite3")
	viper.SetDefault("database.connection", "payments.db")
	viper.SetDefault("cors.allowed_origins", []string{"*"})
	return viper.Unmarshal(&config)
}

// initDB подключается к БД и мигрирует модель Payment.
func initDB() error {
	var err error
	switch config.Database.Dialect {
	case "postgres":
		db, err = gorm.Open(postgres.Open(config.Database.Connection), &gorm.Config{})
	default:
		db, err = gorm.Open(sqlite.Open(config.Database.Connection), &gorm.Config{})
	}
	if err != nil {
		return err
	}
	return db.AutoMigrate(&Payment{})
}

// payHandler инициализирует платёж через Tinkoff.
func payHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	var req PaymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Неверный формат запроса", http.StatusBadRequest)
		return
	}
	// формируем success/fail URLs с никнеймом
	q := url.Values{}
	q.Set("order_id", req.OrderID)
	q.Set("player_name", req.PlayerName)
	sURL := fmt.Sprintf("%s?%s", config.Server.SuccessURL, q.Encode())
	fURL := fmt.Sprintf("%s?%s", config.Server.FailURL, q.Encode())

	initReq := &tinkoff.InitRequest{
		OrderID:         req.OrderID,
		Amount:          req.Amount,
		ClientIP:        req.ClientIP,
		Description:     "",
		Language:        "ru",
		SuccessURL:      sURL,
		FailURL:         fURL,
		NotificationURL: config.Tinkoff.NotificationURL,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := tinkoffClient.InitWithContext(ctx, initReq)
	if err != nil {
		http.Error(w, "Ошибка инициализации", http.StatusInternalServerError)
		return
	}
	// сохраняем в БД
	db.Create(&Payment{OrderID: res.OrderID, PaymentID: res.PaymentID, Amount: req.Amount, PaymentURL: res.PaymentURL, Status: res.Status, PlayerName: req.PlayerName})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(PaymentResponse{PaymentURL: res.PaymentURL})
}

// notifyHandler обрабатывает колбэк и выполняет RCON-команду.
func notifyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	var notif Notification
	if err := json.NewDecoder(r.Body).Decode(&notif); err != nil {
		http.Error(w, "Неверный колбэк", http.StatusBadRequest)
		return
	}
	// обновляем статус
	db.Model(&Payment{}).Where("order_id = ?", notif.OrderID).Updates(Payment{Status: notif.Status})
	if notif.Status == tinkoff.StatusConfirmed {
		// получаем платеж
		var p Payment
		db.Where("order_id = ?", notif.OrderID).First(&p)
		// формируем RCON команду
		cmd := strings.ReplaceAll(config.RCON.Command, "{player}", p.PlayerName)
		// соединяемся и шлём
		rc, err := rcon.Dial(config.RCON.Address, config.RCON.Password)
		if err != nil {
			log.Printf("RCON connect failed: %v", err)
		} else {
			if resp, err := rc.Write(cmd); err != nil {
				log.Printf("RCON write failed: %v", err)
			} else {
				log.Printf("RCON response: %s", resp)
			}
			rc.Close()
		}
		// логиним ник
		log.Printf("[PAYMENT SUCCESS] player: %s", p.PlayerName)
	}
	w.Write([]byte("OK"))
}

func main() {
	// загрузка конфига
	if err := loadConfig(); err != nil {
		log.Fatalf("Config error: %v", err)
	}
	// init DB
	if err := initDB(); err != nil {
		log.Fatalf("DB error: %v", err)
	}
	// Tinkoff клиент
	tinkoffClient = tinkoff.NewClient(config.Tinkoff.TerminalKey, config.Tinkoff.SecretKey)

	mux := http.NewServeMux()
	mux.HandleFunc("/pay", payHandler)
	mux.HandleFunc("/notify", notifyHandler)

	h := cors.New(cors.Options{AllowedOrigins: config.CORS.AllowedOrigins}).Handler(mux)
	srv := &http.Server{Addr: ":" + config.Server.Port, Handler: h}
	// graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		srv.Shutdown(context.Background())
	}()

	log.Printf("Server started on :%s", config.Server.Port)
	srv.ListenAndServe()
	log.Println("Server stopped")
}
