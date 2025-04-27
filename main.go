package main

import (
    "context"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "log"
    "math"
    "net/http"
    "net/url"
    "os"
    "os/signal"
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
}

var (
    config         Config
    db             *gorm.DB
    tinkoffClient  *tinkoff.Client
)

// Payment – модель для хранения информации о платеже.
type Payment struct {
    gorm.Model
    OrderID     string `gorm:"uniqueIndex"`
    PaymentID   string
    Amount      uint64
    Description string
    ClientIP    string
    PaymentURL  string
    Status      string
    PlayerName  string
    Email       string
}

// NewPaymentRequest описывает входящие данные для /pay.
type NewPaymentRequest struct {
    OrderID       string `json:"order_id"`
    Username      string `json:"username"`
    OrderDate     string `json:"order_date"`
    CustomerIP    string `json:"customer_ip"`
    CustomerEmail string `json:"customer_email"`
    Purchase      struct {
        ItemName        string  `json:"item_name"`
        ItemDescription string  `json:"item_description"`
        ItemAmount      float64 `json:"item_amount"`
    } `json:"purchase"`
}

// PaymentResponse возвращается клиенту после инициализации платежа.
type PaymentResponse struct {
    URL     string `json:"url"`
    Message string `json:"message,omitempty"`
}

// TransactionResponse – структура для отправки информации о последней покупке.
type TransactionResponse struct {
    User   string `json:"user"`
    Amount uint64 `json:"amount"` // Сумма в рублях
    Time   string `json:"time"`   // Относительное время
}

type OrderItem struct {
    Product struct {
        Name string
    }
    Price      uint64
    Quantity   int
    TotalPrice uint64
}

func createReceipt(orderItems []OrderItem, email, taxation string) *tinkoff.Receipt {
    var items []*tinkoff.ReceiptItem
    var totalAmount uint64

    for _, item := range orderItems {
        items = append(items, &tinkoff.ReceiptItem{
            Name:     item.Product.Name,
            Price:    item.Price,
            Quantity: fmt.Sprintf("%d", item.Quantity),
            Amount:   item.TotalPrice,
            Tax:      tinkoff.VATNone,
        })
        totalAmount += item.TotalPrice
    }

    return &tinkoff.Receipt{
        Email:    email,
        Taxation: taxation, // "usn_income" или из config.Tinkoff.DefaultTaxation
        Items:    items,
        Payments: &tinkoff.ReceiptPayments{
            Electronic: totalAmount,
        },
    }
}

func loadConfig() error {
    viper.SetConfigName("config")
    viper.SetConfigType("yaml")
    viper.AddConfigPath(".")
    viper.SetDefault("server.port", "8080")
    viper.SetDefault("cors.allowed_origins", []string{"*"})
    if err := viper.ReadInConfig(); err != nil {
        log.Println("Используем дефолтные настройки, так как config.yaml не найден")
    }
    return viper.Unmarshal(&config)
}

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

// payHandler – обработчик инициализации платежа с включением Receipt
func payHandler(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
        return
    }

    // Парсим тело запроса
    var req NewPaymentRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, "Неверный формат запроса", http.StatusBadRequest)
        return
    }

    // Проверяем обязательные поля
    if req.OrderID == "" || req.Username == "" || req.OrderDate == "" ||
        req.CustomerIP == "" || req.CustomerEmail == "" ||
        req.Purchase.ItemName == "" || req.Purchase.ItemDescription == "" {
        http.Error(w, "Отсутствует одно из обязательных полей", http.StatusBadRequest)
        return
    }

    // Переводим сумму из рублей в копейки и проверяем лимиты
    amountKopecks := uint64(math.Round(req.Purchase.ItemAmount * 100))
    const maxAmount = 9_999_999_999
    if amountKopecks == 0 || amountKopecks > maxAmount {
        http.Error(w, fmt.Sprintf("Сумма должна быть от 0.01 до %d копеек", maxAmount), http.StatusBadRequest)
        return
    }

    // Собираем query-параметры для редиректов
    qs := url.Values{}
    qs.Set("username", req.Username)
    qs.Set("order_id", req.OrderID)
    qs.Set("date", req.OrderDate)

    successURL := fmt.Sprintf("%s?%s", config.Server.SuccessURL, qs.Encode())
    failURL    := fmt.Sprintf("%s?%s", config.Server.FailURL,    qs.Encode())

    // Подготовка OrderItem для формирования чека
    orderItems := []OrderItem{{
        Product:    struct{ Name string }{ Name: req.Purchase.ItemName },
        Price:      amountKopecks,
        Quantity:   1,
        TotalPrice: amountKopecks,
    }}

    // Формируем InitRequest с Receipt
    initReq := &tinkoff.InitRequest{
        Amount:          amountKopecks,
        OrderID:         req.OrderID,
        ClientIP:        req.CustomerIP,
        Description:     req.Purchase.ItemDescription,
        Language:        "ru",
        SuccessURL:      successURL,
        FailURL:         failURL,
        NotificationURL: config.Tinkoff.NotificationURL,
        Receipt:         createReceipt(orderItems, req.CustomerEmail, "usn_income"),
    }

    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    initResp, err := tinkoffClient.InitWithContext(ctx, initReq)
    if err != nil {
        log.Printf("Ошибка Init (order_id=%s): %v", req.OrderID, err)
        http.Error(w, "Ошибка инициализации платежа", http.StatusInternalServerError)
        return
    }

    // Сохраняем в БД
    payment := Payment{
        OrderID:     req.OrderID,
        PaymentID:   initResp.PaymentID,
        Amount:      amountKopecks,
        Description: req.Purchase.ItemDescription,
        ClientIP:    req.CustomerIP,
        PaymentURL:  initResp.PaymentURL,
        Status:      "INIT",
        PlayerName:  req.Username,
        Email:       req.CustomerEmail,
    }
    if err := db.Create(&payment).Error; err != nil {
        log.Printf("Ошибка сохранения платежа: %v", err)
    }

    // Отправляем ответ клиенту
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(PaymentResponse{
        URL: initResp.PaymentURL,
    })
}


// notifyHandler – обработчик уведомлений от Tinkoff.
func notifyHandler(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
        return
    }
    var notif struct {
        TerminalKey string      `json:"TerminalKey"`
        OrderID     string      `json:"OrderId"`
        PaymentID   json.Number `json:"PaymentId"`
        Status      string      `json:"Status"`
        ErrorCode   string      `json:"ErrorCode"`
        Message     string      `json:"Message"`
    }
    if err := json.NewDecoder(r.Body).Decode(&notif); err != nil {
        log.Printf("Ошибка декодирования уведомления: %v", err)
        http.Error(w, "Неверный формат уведомления", http.StatusBadRequest)
        return
    }

    var payment Payment
    if err := db.Where("order_id = ?", notif.OrderID).First(&payment).Error; err != nil {
        log.Printf("Платёж не найден: %v", err)
    } else {
        payment.Status = notif.Status
        payment.PaymentID = notif.PaymentID.String()
        if err := db.Save(&payment).Error; err != nil {
            log.Printf("Ошибка обновления платежа: %v", err)
        }
        if notif.Status == "CONFIRMED" {
            processSuccessfulPayment(payment)
        }
    }

    w.WriteHeader(http.StatusOK)
    w.Write([]byte("OK"))
}

// checkHandler – проверка статуса платежа через API Tinkoff.
func checkHandler(w http.ResponseWriter, r *http.Request) {
    orderID := r.URL.Query().Get("order_id")
    if orderID == "" {
        http.Error(w, "Параметр order_id обязателен", http.StatusBadRequest)
        return
    }
    req := &tinkoff.CheckOrderRequest{OrderID: orderID}
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    resp, err := tinkoffClient.CheckOrderWithContext(ctx, req)
    if err != nil {
        log.Printf("Ошибка проверки заказа: %v", err)
        http.Error(w, "Ошибка проверки заказа", http.StatusInternalServerError)
        return
    }
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(resp)
}

// processSuccessfulPayment – заглушка для бизнес-логики.
func processSuccessfulPayment(p Payment) {
    log.Printf("Платёж успешно завершён: OrderID=%s, PaymentID=%s, User=%s, Email=%s",
        p.OrderID, p.PaymentID, p.PlayerName, p.Email)
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

// transactionsHandler – последние 7 подтверждённых платежей.
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

    var out []TransactionResponse
    for _, p := range payments {
        out = append(out, TransactionResponse{
            User:   p.PlayerName,
            Amount: p.Amount / 100,
            Time:   relativeTime(p.CreatedAt),
        })
    }
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(out)
}

func generateToken(params map[string]string, password string) string {
    concat := ""
    for _, v := range params {
        concat += v
    }
    concat += password
    hash := sha256.Sum256([]byte(concat))
    return hex.EncodeToString(hash[:])
}

func main() {
        fmt.Println(">>> main() стартовал")
    if err := loadConfig(); err != nil {
        log.Fatalf("Ошибка загрузки конфигурации: %v", err)
    }
    if err := initDB(); err != nil {
        log.Fatalf("Ошибка инициализации БД: %v", err)
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

    srv := &http.Server{
        Addr:         ":" + config.Server.Port,
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

    log.Printf("Сервис обработки платежей запущен на %s", srv.Addr)
    if err := srv.ListenAndServe(); err != http.ErrServerClosed {
        log.Fatalf("Ошибка запуска сервера: %v", err)
    }
    <-idleConnsClosed
    log.Println("Сервер успешно остановлен.")
}
