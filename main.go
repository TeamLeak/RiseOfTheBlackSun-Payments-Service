package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

	"github.com/nikita-vanyasin/tinkoff"
	"github.com/rs/cors"
	"github.com/spf13/viper"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Config описывает структуру конфигурационного файла.
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
		DefaultTaxation string `mapstructure:"default_taxation"`
	} `mapstructure:"tinkoff"`
	Database struct {
		Dialect    string `mapstructure:"dialect"`
		Connection string `mapstructure:"connection"`
	} `mapstructure:"database"`
	CORS struct {
		AllowedOrigins []string `mapstructure:"allowed_origins"`
	} `mapstructure:"cors"`
	ExternalService struct {
		Domain string `mapstructure:"domain"`
	} `mapstructure:"external_service"`
}

var (
	config        Config
	db            *gorm.DB
	tinkoffClient *tinkoff.Client
)

// Payment – модель для хранения информации о платеже.
type Payment struct {
	gorm.Model
	OrderID      string `gorm:"uniqueIndex"`
	PaymentID    string
	Amount       uint64
	Description  string
	ClientIP     string
	PaymentURL   string
	Status       string
	PlayerName   string
	Email        string
	ProductImage string
	ReceiptItems string
}

// PaymentRequest описывает входящие данные для инициализации платежа.
type PaymentRequest struct {
	OrderID      string  `json:"order_id"`
	Amount       uint64  `json:"amount"`
	Description  string  `json:"description"`
	ClientIP     string  `json:"client_ip"`
	PlayerName   string  `json:"player_name"`
	Email        string  `json:"email"`
	ProductImage string  `json:"product_image"`
	Receipt      Receipt `json:"receipt"`
}

// Receipt описывает данные чека.
type Receipt struct {
	Email    string        `json:"Email"`
	Taxation string        `json:"Taxation"`
	Items    []ReceiptItem `json:"Items"`
}

// ReceiptItem описывает одну позицию в чеке.
type ReceiptItem struct {
	Name     string `json:"Name"`
	Price    uint64 `json:"Price"`
	Quantity string `json:"Quantity"`
	Amount   uint64 `json:"Amount"`
	Tax      string `json:"Tax"`
}

// PaymentResponse возвращается клиенту после инициализации платежа.
type PaymentResponse struct {
	PaymentURL string `json:"payment_url"`
	Message    string `json:"message,omitempty"`
}

// PaymentNotificationData – структура уведомления от Tinkoff.
type PaymentNotificationData struct {
	TerminalKey string      `json:"TerminalKey"`
	OrderID     string      `json:"OrderId"`
	PaymentID   json.Number `json:"PaymentId"`
	Status      string      `json:"Status"`
	ErrorCode   string      `json:"ErrorCode"`
	Message     string      `json:"Message"`
}

// loadConfig загружает конфигурацию из файла config.yaml.
func loadConfig() error {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	// Дефолтные настройки
	viper.SetDefault("server.port", "8080")
	viper.SetDefault("server.success_url", "https://example.com/success")
	viper.SetDefault("server.fail_url", "https://example.com/fail")

	viper.SetDefault("tinkoff.terminal_key", "")
	viper.SetDefault("tinkoff.secret_key", "")
	viper.SetDefault("tinkoff.notification_url", "")
	viper.SetDefault("tinkoff.default_taxation", "osn")

	viper.SetDefault("database.dialect", "sqlite3")
	viper.SetDefault("database.connection", "payments.db")

	viper.SetDefault("cors.allowed_origins", []string{"*"})

	viper.SetDefault("external_service.domain", "")

	if err := viper.ReadInConfig(); err != nil {
		log.Println("Используем дефолтные настройки, так как config.yaml не найден")
	}
	return viper.Unmarshal(&config)
}

// initDB открывает соединение с базой данных.
func initDB() error {
	var err error
	switch config.Database.Dialect {
	case "sqlite3":
		db, err = gorm.Open(sqlite.Open(config.Database.Connection), &gorm.Config{})
	case "postgres":
		db, err = gorm.Open(postgres.Open(config.Database.Connection), &gorm.Config{})
	default:
		return fmt.Errorf("unsupported database dialect: %s", config.Database.Dialect)
	}
	if err != nil {
		return err
	}
	return db.AutoMigrate(&Payment{})
}

// convertReceiptItems конвертирует позиции чека в формат tinkoff.
func convertReceiptItems(items []ReceiptItem) []*tinkoff.ReceiptItem {
	var res []*tinkoff.ReceiptItem
	for _, item := range items {
		res = append(res, &tinkoff.ReceiptItem{
			Name:     item.Name,
			Price:    item.Price,
			Quantity: item.Quantity,
			Amount:   item.Amount,
			Tax:      tinkoff.VATNone,
		})
	}
	return res
}

// convertReceipt преобразует Receipt в структуру tinkoff.Receipt.
func convertReceipt(r Receipt, totalAmount uint64) *tinkoff.Receipt {
	return &tinkoff.Receipt{
		Email:    r.Email,
		Phone:    "",
		Taxation: r.Taxation,
		Items:    convertReceiptItems(r.Items),
		Payments: &tinkoff.ReceiptPayments{
			Electronic: totalAmount,
		},
	}
}

// payHandler – обработчик инициализации платежа.
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
	// валидация
	if req.OrderID == "" || req.Amount == 0 || req.ClientIP == "" || req.PlayerName == "" || req.Email == "" {
		http.Error(w, "Поля order_id, amount, client_ip, player_name и email обязательны", http.StatusBadRequest)
		return
	}
	// формируем URLs с передачей player_name
	q := url.Values{}
	q.Set("order_id", req.OrderID)
	q.Set("player_name", req.PlayerName)
	successURL := fmt.Sprintf("%s?%s", config.Server.SuccessURL, q.Encode())
	failURL := fmt.Sprintf("%s?%s", config.Server.FailURL, q.Encode())

	initReq := &tinkoff.InitRequest{
		Amount:          req.Amount,
		OrderID:         req.OrderID,
		ClientIP:        req.ClientIP,
		Description:     req.Description,
		Language:        "ru",
		SuccessURL:      successURL,
		FailURL:         failURL,
		NotificationURL: config.Tinkoff.NotificationURL,
		Receipt:         convertReceipt(req.Receipt, req.Amount),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	initResp, err := tinkoffClient.InitWithContext(ctx, initReq)
	if err != nil {
		log.Printf("Ошибка инициализации платежа: %v", err)
		http.Error(w, "Ошибка инициализации платежа", http.StatusInternalServerError)
		return
	}

	// сохраняем в БД
	receiptItemsJSON, _ := json.Marshal(req.Receipt.Items)
	payment := Payment{
		OrderID:      req.OrderID,
		PaymentID:    initResp.PaymentID,
		Amount:       req.Amount,
		Description:  req.Description,
		ClientIP:     req.ClientIP,
		PaymentURL:   initResp.PaymentURL,
		Status:       "INIT",
		PlayerName:   req.PlayerName,
		Email:        req.Email,
		ProductImage: req.ProductImage,
		ReceiptItems: string(receiptItemsJSON),
	}
	db.Create(&payment)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(PaymentResponse{PaymentURL: initResp.PaymentURL})
}

// notifyHandler – обработчик уведомлений от Tinkoff.
func notifyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	var notif PaymentNotificationData
	if err := json.NewDecoder(r.Body).Decode(&notif); err != nil {
		log.Printf("Ошибка декодирования уведомления: %v", err)
		http.Error(w, "Неверный формат уведомления", http.StatusBadRequest)
		return
	}

	// находим платеж
	var payment Payment
	if err := db.Where("order_id = ?", notif.OrderID).First(&payment).Error; err != nil {
		log.Printf("Платеж не найден: %v", err)
	} else {
		payment.Status = notif.Status
		payment.PaymentID = notif.PaymentID.String()
		db.Save(&payment)

		// если подтверждён — показываем никнейм
		if notif.Status == tinkoff.StatusConfirmed {
			fmt.Printf("[SUCCESS] PlayerName: %q\n", payment.PlayerName)
		}
	}

	w.Write([]byte("OK"))
}

// checkHandler – проверка статуса на запросе.
func checkHandler(w http.ResponseWriter, r *http.Request) {
	orderID := r.URL.Query().Get("order_id")
	if orderID == "" {
		http.Error(w, "order_id обязателен", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := tinkoffClient.CheckOrderWithContext(ctx, &tinkoff.CheckOrderRequest{OrderID: orderID})
	if err != nil {
		http.Error(w, "Ошибка проверки", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func main() {
	// загрузка конфигурации
	if err := loadConfig(); err != nil {
		log.Fatalf("config load error: %v", err)
	}
	// инициализация БД
	if err := initDB(); err != nil {
		log.Fatalf("db init error: %v", err)
	}
	// клиент Тинькофф
	tinkoffClient = tinkoff.NewClient(config.Tinkoff.TerminalKey, config.Tinkoff.SecretKey)

	// маршруты
	mux := http.NewServeMux()
	mux.HandleFunc("/pay", payHandler)
	mux.HandleFunc("/notify", notifyHandler)
	mux.HandleFunc("/check", checkHandler)

	c := cors.New(cors.Options{
		AllowedOrigins:   config.CORS.AllowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type"},
		AllowCredentials: true,
	})
	handler := c.Handler(mux)

	// graceful shutdown
	server := &http.Server{
		Addr:    ":" + config.Server.Port,
		Handler: handler,
	}
	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		server.Shutdown(context.Background())
		close(idle)
	}()

	log.Printf("Сервер запущен на порту %s", config.Server.Port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("ListenAndServe: %v", err)
	}
	<-idle
	log.Println("Сервер остановлен")
}
