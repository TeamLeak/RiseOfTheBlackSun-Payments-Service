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
	"github.com/streadway/amqp"
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
	// Внешний сервис, к которому можно обращаться через HTTP.
	ExternalService struct {
		Domain string `mapstructure:"domain"`
	} `mapstructure:"external_service"`
	// Параметры подключения к RabbitMQ.
	RabbitMQ struct {
		URL   string `mapstructure:"url"`
		Queue string `mapstructure:"queue"`
	} `mapstructure:"rabbitmq"`
}

var config Config
var db *gorm.DB
var tinkoffClient *tinkoff.Client

// Глобальные переменные для RabbitMQ.
var rabbitConn *amqp.Connection
var rabbitChannel *amqp.Channel

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
	// Купленные товары (позиции чека) сохраняются в виде JSON-строки.
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

// TransactionResponse – информация о последней покупке.
type TransactionResponse struct {
	User   string `json:"user"`
	Amount uint64 `json:"amount"`
	Time   string `json:"time"`
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

	viper.SetDefault("tinkoff.terminal_key", "1234567890")
	viper.SetDefault("tinkoff.secret_key", "SECRET")
	viper.SetDefault("tinkoff.notification_url", "https://example.com/notify")
	viper.SetDefault("tinkoff.default_taxation", "osn")

	viper.SetDefault("database.dialect", "sqlite3")
	viper.SetDefault("database.connection", "payments.db")

	viper.SetDefault("cors.allowed_origins", []string{"*"})

	// URL внешнего сервиса
	viper.SetDefault("external_service.domain", "https://shopservice.riseoftheblacksun.eu/api/process")

	// Конфигурация RabbitMQ
	viper.SetDefault("rabbitmq.url", "amqp://guest:guest@localhost:5672/")
	viper.SetDefault("rabbitmq.queue", "purchase-event")

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

// initRabbitMQ устанавливает подключение к RabbitMQ, используя конфигурацию.
func initRabbitMQ() error {
	var err error
	rabbitConn, err = amqp.Dial(config.RabbitMQ.URL)
	if err != nil {
		return fmt.Errorf("failed to connect to RabbitMQ: %v", err)
	}
	rabbitChannel, err = rabbitConn.Channel()
	if err != nil {
		return fmt.Errorf("failed to open RabbitMQ channel: %v", err)
	}
	return nil
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

	if req.OrderID == "" || req.Amount == 0 || req.ClientIP == "" || req.PlayerName == "" || req.Email == "" {
		http.Error(w, "Поля order_id, amount, client_ip, player_name и email обязательны", http.StatusBadRequest)
		return
	}

	const maxAmount uint64 = 9999999999
	if req.Amount > maxAmount {
		http.Error(w, fmt.Sprintf("Сумма платежа не должна превышать %d копеек", maxAmount), http.StatusBadRequest)
		return
	}

	q := url.Values{}
	q.Set("order_id", req.OrderID)
	q.Set("player_name", req.PlayerName)
	q.Set("email", req.Email)
	q.Set("desc", req.Description)
	q.Set("product_image", req.ProductImage)
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
		log.Printf("Ошибка инициализации платежа (order_id=%s): %v", req.OrderID, err)
		http.Error(w, "Ошибка инициализации платежа", http.StatusInternalServerError)
		return
	}

	// Сохраняем купленные товары (позиции чека) в виде JSON.
	receiptItemsJSON, err := json.Marshal(req.Receipt.Items)
	if err != nil {
		log.Printf("Ошибка сериализации receipt items: %v", err)
	}

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
	if err := db.Create(&payment).Error; err != nil {
		log.Printf("Ошибка сохранения платежа в БД: %v", err)
	}

	resp := PaymentResponse{
		PaymentURL: initResp.PaymentURL,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
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
	log.Printf("Получено уведомление: %+v", notif)

	var payment Payment
	if err := db.Where("order_id = ?", notif.OrderID).First(&payment).Error; err != nil {
		log.Printf("Платеж с order_id=%s не найден: %v", notif.OrderID, err)
	} else {
		payment.Status = notif.Status
		payment.PaymentID = notif.PaymentID.String()
		if err := db.Save(&payment).Error; err != nil {
			log.Printf("Ошибка обновления платежа в БД: %v", err)
		}
		if notif.Status == "CONFIRMED" {
			processSuccessfulPayment(payment)
		}
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// checkHandler – обработчик проверки статуса платежа.
func checkHandler(w http.ResponseWriter, r *http.Request) {
	orderID := r.URL.Query().Get("order_id")
	if orderID == "" {
		http.Error(w, "Параметр order_id обязателен", http.StatusBadRequest)
		return
	}
	checkReq := &tinkoff.CheckOrderRequest{
		OrderID: orderID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checkResp, err := tinkoffClient.CheckOrderWithContext(ctx, checkReq)
	if err != nil {
		log.Printf("Ошибка проверки заказа (order_id=%s): %v", orderID, err)
		http.Error(w, "Ошибка проверки заказа", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(checkResp)
}

// processSuccessfulPayment – обработчик успешного платежа.
// Извлекает последний подтверждённый платеж для пользователя, формирует событие
// с данными (включая serverID, определяемый как часть order_id до первого "-"),
// сериализует тело (с командами и предметами) в base64 и отправляет событие в RabbitMQ.
func processSuccessfulPayment(payment Payment) {
	log.Printf("Платеж успешно завершён: OrderID=%s, PaymentID=%s, Player=%s, Email=%s",
		payment.OrderID, payment.PaymentID, payment.PlayerName, payment.Email)

	// Выбираем последний успешный платеж для данного пользователя.
	var lastPayment Payment
	if err := db.Where("email = ? AND status = ?", payment.Email, "CONFIRMED").
		Order("created_at desc").
		First(&lastPayment).Error; err != nil {
		log.Printf("Ошибка получения последнего платежа для %s: %v", payment.Email, err)
		return
	}

	// Извлекаем список купленных товаров.
	var items []ReceiptItem
	if err := json.Unmarshal([]byte(lastPayment.ReceiptItems), &items); err != nil {
		log.Printf("Ошибка разбора купленных товаров: %v", err)
		return
	}

	// Пример вызова внешнего сервиса (HTTP POST) – если необходимо.
	payload := map[string]interface{}{
		"order_id":    lastPayment.OrderID,
		"player_name": lastPayment.PlayerName,
		"email":       lastPayment.Email,
		"items":       items,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Ошибка сериализации данных для внешнего сервиса: %v", err)
		return
	}
	extReq, err := http.NewRequest("POST", config.ExternalService.Domain, bytes.NewBuffer(payloadBytes))
	if err != nil {
		log.Printf("Ошибка создания запроса к внешнему сервису: %v", err)
		return
	}
	extReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	extResp, err := client.Do(extReq)
	if err != nil {
		log.Printf("Ошибка выполнения запроса к внешнему сервису: %v", err)
		return
	}
	extResp.Body.Close()
	log.Printf("Внешний сервис ответил со статусом: %s", extResp.Status)

	// Формируем событие для RabbitMQ.
	// Определяем serverID как часть order_id до первого "-"
	serverID := lastPayment.OrderID
	if idx := strings.Index(serverID, "-"); idx != -1 {
		serverID = serverID[:idx]
	}

	// Формируем тело события (например, с командами и предметами).
	eventBody := map[string]interface{}{
		"commands": []string{"[player] help", fmt.Sprintf("[console] op %s", lastPayment.PlayerName)},
		"items":    []string{"base64"},
	}
	bodyBytes, err := json.Marshal(eventBody)
	if err != nil {
		log.Printf("Ошибка сериализации тела события: %v", err)
		return
	}
	bodyBase64 := base64.StdEncoding.EncodeToString(bodyBytes)

	// Формируем само событие.
	purchaseEvent := map[string]interface{}{
		"serverID":   serverID,
		"playerName": lastPayment.PlayerName,
		"body":       bodyBase64,
	}
	eventBytes, err := json.Marshal(purchaseEvent)
	if err != nil {
		log.Printf("Ошибка сериализации события покупки: %v", err)
		return
	}

	// Публикуем событие в RabbitMQ.
	err = rabbitChannel.Publish(
		"",                    // используем дефолтный exchange
		config.RabbitMQ.Queue, // routing key из конфигурации
		false,
		false,
		amqp.Publishing{
			ContentType: "application/json",
			Body:        eventBytes,
		},
	)
	if err != nil {
		log.Printf("Ошибка публикации в RabbitMQ: %v", err)
	} else {
		log.Printf("Событие покупки отправлено в RabbitMQ: %s", string(eventBytes))
	}
}

// generateToken – пример генерации токена.
func generateToken(params map[string]string, password string) string {
	concat := ""
	for _, v := range params {
		concat += v
	}
	concat += password
	hash := sha256.Sum256([]byte(concat))
	return hex.EncodeToString(hash[:])
}

// relativeTime возвращает относительное время.
func relativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		return fmt.Sprintf("%d минут назад", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d часов назад", int(d.Hours()))
	default:
		return fmt.Sprintf("%d дней назад", int(d.Hours()/24))
	}
}

// transactionsHandler – возвращает последние 7 успешных покупок.
func transactionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	var payments []Payment
	if err := db.Where("status = ?", "CONFIRMED").
		Order("created_at desc").
		Limit(7).
		Find(&payments).Error; err != nil {
		log.Printf("Ошибка получения транзакций: %v", err)
		http.Error(w, "Ошибка получения транзакций", http.StatusInternalServerError)
		return
	}
	var transactions []TransactionResponse
	for _, p := range payments {
		transactions = append(transactions, TransactionResponse{
			User:   p.PlayerName,
			Amount: p.Amount / 100,
			Time:   relativeTime(p.CreatedAt),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(transactions)
}

func main() {
	if err := loadConfig(); err != nil {
		log.Fatalf("Ошибка загрузки конфигурации: %v", err)
	}
	if err := initDB(); err != nil {
		log.Fatalf("Ошибка инициализации БД: %v", err)
	}
	if err := initRabbitMQ(); err != nil {
		log.Fatalf("Ошибка подключения к RabbitMQ: %v", err)
	}
	tinkoffClient = tinkoff.NewClient(config.Tinkoff.TerminalKey, config.Tinkoff.SecretKey)

	mux := http.NewServeMux()
	mux.HandleFunc("/pay", payHandler)
	mux.HandleFunc("/notify", notifyHandler)
	mux.HandleFunc("/check", checkHandler)
	mux.HandleFunc("/transactions", transactionsHandler)
	c := cors.New(cors.Options{
		AllowedOrigins:   config.CORS.AllowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type", "Authorization"},
		AllowCredentials: true,
	})
	handler := c.Handler(mux)

	serverAddr := ":" + config.Server.Port
	srv := &http.Server{
		Addr:         serverAddr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, syscall.SIGINT, syscall.SIGTERM)
		<-sigint
		log.Println("Получен сигнал завершения. Останавливаем сервер...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("Ошибка при завершении работы сервера: %v", err)
		}
		close(idleConnsClosed)
	}()

	log.Printf("Сервис обработки платежей запущен на %s", serverAddr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Ошибка запуска сервера: %v", err)
	}
	<-idleConnsClosed
	log.Println("Сервер успешно остановлен.")
}
