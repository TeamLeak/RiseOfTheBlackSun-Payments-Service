package main

import (
        "context"
        "crypto/sha256"
        "encoding/hex"
        "encoding/json"
        "fmt"
        "log"
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
                // Здесь задаётся система налогообложения, например "osn"
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

var config Config
var db *gorm.DB
var tinkoffClient *tinkoff.Client

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
}

// PaymentRequest описывает входящие данные для инициализации платежа.
type PaymentRequest struct {
        OrderID      string  `json:"order_id"`      // Обязательное поле
        Amount       uint64  `json:"amount"`        // Сумма в копейках
        Description  string  `json:"description"`   // Описание заказа
        ClientIP     string  `json:"client_ip"`     // IP покупателя
        PlayerName   string  `json:"player_name"`   // Ник игрока
        Email        string  `json:"email"`         // Почта игрока
        ProductImage string  `json:"product_image"` // URL картинки товара (опционально)
        Receipt      Receipt `json:"receipt"`       // Чек
}

// Receipt описывает данные чека, которые передаются в Init-запрос.
// Поле Taxation задаёт систему налогообложения (например, "osn").
type Receipt struct {
        Email    string        `json:"Email"`    // Email для отправки чека
        Taxation string        `json:"Taxation"` // Система налогообложения (например, "osn")
        Items    []ReceiptItem `json:"Items"`    // Список позиций чека
}

// ReceiptItem описывает одну позицию в чеке.
type ReceiptItem struct {
        Name     string `json:"Name"`     // Наименование товара
        Price    uint64 `json:"Price"`    // Цена за единицу (в копейках)
        Quantity string `json:"Quantity"` // Количество (например, "1")
        Amount   uint64 `json:"Amount"`   // Итоговая сумма позиции (в копейках)
        // Для товаров без НДС передаём "none"
        Tax string `json:"Tax"`
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

// loadConfig загружает конфигурацию из файла config.yaml или использует дефолтные настройки.
func loadConfig() error {
        viper.SetConfigName("config")
        viper.SetConfigType("yaml")
        viper.AddConfigPath(".")
        // Дефолтные настройки для демонстрации
        viper.SetDefault("server.port", "8080")
        viper.SetDefault("server.success_url", "https://example.com/success")
        viper.SetDefault("server.fail_url", "https://example.com/fail")

        viper.SetDefault("tinkoff.terminal_key", "1234567890")
        viper.SetDefault("tinkoff.secret_key", "SECRET")
        viper.SetDefault("tinkoff.notification_url", "https://example.com/notify")
        // Система налогообложения – передаётся как есть, а для отдельных товаров ставим Tax = "none"
        viper.SetDefault("tinkoff.default_taxation", "osn")

        viper.SetDefault("database.dialect", "sqlite3")
        viper.SetDefault("database.connection", "payments.db")

        viper.SetDefault("cors.allowed_origins", []string{"*"})

        if err := viper.ReadInConfig(); err != nil {
                log.Println("Используем дефолтные настройки, так как config.yaml не найден")
        }
        return viper.Unmarshal(&config)
}

// initDB открывает соединение с БД по указанному драйверу и DSN.
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
        // Миграция модели Payment.
        return db.AutoMigrate(&Payment{})
}

// convertReceiptItems конвертирует срез ReceiptItem из запроса в срез, ожидаемый библиотекой tinkoff.
// Здесь для каждой позиции устанавливаем Tax равным tinkoff.VATNone ("none") для товаров без НДС.
func convertReceiptItems(items []ReceiptItem) []*tinkoff.ReceiptItem {
        var res []*tinkoff.ReceiptItem
        for _, item := range items {
                res = append(res, &tinkoff.ReceiptItem{
                        Name:     item.Name,
                        Price:    item.Price,
                        Quantity: item.Quantity,
                        Amount:   item.Amount,
                        Tax:      tinkoff.VATNone, // Используем "none" для товаров без НДС
                })
        }
        return res
}

// convertReceipt преобразует наш Receipt в структуру tinkoff.Receipt.
func convertReceipt(r Receipt, totalAmount uint64) *tinkoff.Receipt {
        return &tinkoff.Receipt{
                Email:    r.Email,
                Phone:    "", // телефон не передаётся
                Taxation: r.Taxation, // система налогообложения, например "osn"
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

        // Проверяем обязательные поля.
        if req.OrderID == "" || req.Amount == 0 || req.ClientIP == "" ||
                req.PlayerName == "" || req.Email == "" {
                http.Error(w, "Поля order_id, amount, client_ip, player_name и email обязательны", http.StatusBadRequest)
                return
        }

        // Проверяем, что сумма не превышает максимально допустимое значение (10 цифр, максимум 9,999,999,999)
        const maxAmount uint64 = 9999999999
        if req.Amount > maxAmount {
                http.Error(w, fmt.Sprintf("Сумма платежа не должна превышать %d копеек", maxAmount), http.StatusBadRequest)
                return
        }

        // Формируем query-параметры для редиректа с данными товара.
        q := url.Values{}
        q.Set("order_id", req.OrderID)
        q.Set("player_name", req.PlayerName)
        q.Set("email", req.Email)
        q.Set("desc", req.Description)
        q.Set("product_image", req.ProductImage)
        successURL := fmt.Sprintf("%s?%s", config.Server.SuccessURL, q.Encode())
        failURL := fmt.Sprintf("%s?%s", config.Server.FailURL, q.Encode())

        // Формируем запрос к Tinkoff API для инициализации платежа, включая объект Receipt.
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

        // Сохраняем данные платежа в БД.
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

        // Обновляем статус платежа в БД по OrderID.
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

// processSuccessfulPayment – заглушка для обработки успешного платежа.
func processSuccessfulPayment(payment Payment) {
        log.Printf("Платеж успешно завершён: OrderID=%s, PaymentID=%s, Player=%s, Email=%s. Выполняю дальнейшие действия...",
                payment.OrderID, payment.PaymentID, payment.PlayerName, payment.Email)
        // Здесь можно добавить бизнес-логику: обновление заказа, уведомление клиента и т.д.
}

// generateToken – пример генерации токена, если потребуется.
func generateToken(params map[string]string, password string) string {
        concat := ""
        for _, v := range params {
                concat += v
        }
        concat += password
        hash := sha256.Sum256([]byte(concat))
        return hex.EncodeToString(hash[:])
}

// TransactionResponse – структура для отправки информации о последней покупке.
type TransactionResponse struct {
    User   string `json:"user"`
    Amount uint64 `json:"amount"` // Сумма в рублях
    Time   string `json:"time"`   // Относительное время, например "10 минут назад"
}

// relativeTime возвращает строку с относительным временем от указанного момента до текущего.
func relativeTime(t time.Time) string {
    d := time.Since(t)
    if d < time.Minute {
        return "только что"
    } else if d < time.Hour {
        minutes := int(d.Minutes())
        return fmt.Sprintf("%d минут назад", minutes)
    } else if d < 24*time.Hour {
        hours := int(d.Hours())
        return fmt.Sprintf("%d часов назад", hours)
    }
    days := int(d.Hours() / 24)
    return fmt.Sprintf("%d дней назад", days)
}

// transactionsHandler – обработчик для получения последних 7 успешных покупок.
func transactionsHandler(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
        http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
        return
    }

    var payments []Payment
    // Фильтруем по успешным транзакциям, сортируем по дате создания (от новых к старым)
    // и ограничиваем результат 7 записями.
    if err := db.Where("status = ?", "CONFIRMED").
        Order("created_at desc").
        Limit(7).
        Find(&payments).Error; err != nil {
        log.Printf("Ошибка получения транзакций: %v", err)
        http.Error(w, "Ошибка получения транзакций", http.StatusInternalServerError)
        return
    }

    // Преобразуем платежи в структуру TransactionResponse.
    var transactions []TransactionResponse
    for _, p := range payments {
        // Предполагаем, что поле Amount хранится в копейках, переводим в рубли.
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
        // Загрузка конфигурации.
        if err := loadConfig(); err != nil {
                log.Fatalf("Ошибка загрузки конфигурации: %v", err)
        }
        // Инициализация базы данных.
        if err := initDB(); err != nil {
                log.Fatalf("Ошибка инициализации БД: %v", err)
        }
        // Инициализация клиента Tinkoff.
        tinkoffClient = tinkoff.NewClient(config.Tinkoff.TerminalKey, config.Tinkoff.SecretKey)

        mux := http.NewServeMux()
        mux.HandleFunc("/pay", payHandler)
        mux.HandleFunc("/notify", notifyHandler)
        mux.HandleFunc("/check", checkHandler)
        mux.HandleFunc("/transactions", transactionsHandler) // Новый маршрут для последних покупок
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