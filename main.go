package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/nikita-vanyasin/tinkoff"
	"github.com/rs/cors"
	"github.com/spf13/viper"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Модели базы данных
type Shop struct {
	gorm.Model
	UUID        string `gorm:"uniqueIndex"`
	Name        string
	Description string
	Active      bool
	Terminals   []Terminal
	Products    []Product
}

type Terminal struct {
	gorm.Model
	UUID           string `gorm:"uniqueIndex"`
	Name           string
	TerminalKey    string
	SecretKey      string
	NotificationURL string
	Active         bool
	ShopID         uint
	Shop           Shop
}

type Product struct {
	gorm.Model
	UUID        string `gorm:"uniqueIndex"`
	SKU         string
	Name        string
	Description string
	Price       uint64
	Image       string
	Active      bool
	ShopID      uint
	Shop        Shop
}

type Payment struct {
	gorm.Model
	UUID        string `gorm:"uniqueIndex"`
	OrderID     string `gorm:"uniqueIndex"`
	PaymentID   string
	Amount      uint64
	Status      string
	PlayerName  string
	Email       string
	ShopID      uint
	Shop        Shop
	TerminalID  uint
	Terminal    Terminal
	Items       []OrderItem
}

type OrderItem struct {
	gorm.Model
	UUID       string `gorm:"uniqueIndex"`
	PaymentID  uint
	Payment    Payment
	ProductID  uint
	Product    Product
	Quantity   int
	Price      uint64
	TotalPrice uint64
}

// Структуры конфигурации
type Config struct {
	Server struct {
		Port         string `mapstructure:"port"`
		Workers      int    `mapstructure:"workers"`
		ReadTimeout  int    `mapstructure:"read_timeout"`
		WriteTimeout int    `mapstructure:"write_timeout"`
	} `mapstructure:"server"`

	Database struct {
		Dialect    string `mapstructure:"dialect"`
		Connection string `mapstructure:"connection"`
		LogMode    bool   `mapstructure:"log_mode"`
	} `mapstructure:"database"`

	CORS struct {
		AllowedOrigins []string `mapstructure:"allowed_origins"`
		AllowedMethods []string `mapstructure:"allowed_methods"`
	} `mapstructure:"cors"`

	ExternalService struct {
		Domain     string `mapstructure:"domain"`
		MaxRetries int    `mapstructure:"max_retries"`
		Timeout    int    `mapstructure:"timeout"`
	} `mapstructure:"external_service"`

	Shops []ShopConfig `mapstructure:"shops"`
}

type ShopConfig struct {
	UUID        string          `mapstructure:"uuid"`
	Name        string          `mapstructure:"name"`
	Description string          `mapstructure:"description"`
	Terminals   []TerminalConfig `mapstructure:"terminals"`
	Products    []ProductConfig  `mapstructure:"products"`
}

type TerminalConfig struct {
	UUID            string `mapstructure:"uuid"`
	Name            string `mapstructure:"name"`
	TerminalKey     string `mapstructure:"terminal_key"`
	SecretKey       string `mapstructure:"secret_key"`
	NotificationURL string `mapstructure:"notification_url"`
	SuccessURL      string `mapstructure:"success_url"`
	FailURL         string `mapstructure:"fail_url"`
}

type ProductConfig struct {
	UUID        string `mapstructure:"uuid"`
	SKU         string `mapstructure:"sku"`
	Name        string `mapstructure:"name"`
	Description string `mapstructure:"description"`
	Price       uint64 `mapstructure:"price"`
	Image       string `mapstructure:"image"`
}

// Запросы и ответы API
type PaymentRequest struct {
	ShopID     string              `json:"shop_id"`
	OrderID    string              `json:"order_id"`
	Products   []OrderProductRequest `json:"products"`
	ClientIP   string              `json:"client_ip"`
	PlayerName string              `json:"player_name"`
	Email      string              `json:"email"`
}

type OrderProductRequest struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
}

type PaymentResponse struct {
	PaymentURL string `json:"payment_url"`
	OrderID    string `json:"order_id"`
	Message    string `json:"message,omitempty"`
}

type PaymentNotificationData struct {
	TerminalKey string      `json:"TerminalKey"`
	OrderID     string      `json:"OrderId"`
	PaymentID   json.Number `json:"PaymentId"`
	Status      string      `json:"Status"`
}

type ShopResponse struct {
	UUID        string           `json:"uuid"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Products    []ProductResponse `json:"products"`
}

type ProductResponse struct {
	UUID        string `json:"uuid"`
	SKU         string `json:"sku"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Price       uint64 `json:"price"`
	Image       string `json:"image"`
}

// Глобальные переменные
var (
	config     Config
	db         *gorm.DB
	shopsMutex sync.RWMutex
	shopCache  map[string]*Shop
	sem        *semaphore.Weighted
)

// Сервис для работы с терминалами Tinkoff
type TinkoffService struct {
	clientCache map[string]*tinkoff.Client
	mu          sync.RWMutex
}

func NewTinkoffService() *TinkoffService {
	return &TinkoffService{
		clientCache: make(map[string]*tinkoff.Client),
	}
}

func (ts *TinkoffService) GetClient(terminalKey, secretKey string) *tinkoff.Client {
	ts.mu.RLock()
	client, exists := ts.clientCache[terminalKey]
	ts.mu.RUnlock()

	if !exists {
		client = tinkoff.NewClient(terminalKey, secretKey)
		ts.mu.Lock()
		ts.clientCache[terminalKey] = client
		ts.mu.Unlock()
	}

	return client
}

// Инициализация конфигурации
func loadConfig() error {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")

	setDefaults()

	if err := viper.ReadInConfig(); err != nil {
		log.Println("Warning: Using default config")
	}

	if err := viper.Unmarshal(&config); err != nil {
		return err
	}

	return nil
}

func setDefaults() {
	viper.SetDefault("server.port", "8080")
	viper.SetDefault("server.workers", 10)
	viper.SetDefault("server.read_timeout", 10)
	viper.SetDefault("server.write_timeout", 10)
	
	viper.SetDefault("database.dialect", "sqlite3")
	viper.SetDefault("database.connection", "payments.db")
	viper.SetDefault("database.log_mode", false)
	
	viper.SetDefault("cors.allowed_origins", []string{"*"})
	viper.SetDefault("cors.allowed_methods", []string{"GET", "POST", "OPTIONS"})
	
	viper.SetDefault("external_service.max_retries", 3)
	viper.SetDefault("external_service.timeout", 10)
}

// Инициализация базы данных
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

	// Автомиграция схемы БД
	if err := db.AutoMigrate(&Shop{}, &Terminal{}, &Product{}, &Payment{}, &OrderItem{}); err != nil {
		return err
	}

	// Инициализация кэша магазинов
	shopCache = make(map[string]*Shop)

	return nil
}

// Загрузка и обновление данных из конфигурации
func loadShops() error {
	for _, shopConfig := range config.Shops {
		var shop Shop
		result := db.Where("uuid = ?", shopConfig.UUID).First(&shop)
		if result.Error != nil && result.Error != gorm.ErrRecordNotFound {
			return result.Error
		}

		isNewShop := result.Error == gorm.ErrRecordNotFound
		if isNewShop {
			shop = Shop{
				UUID:        shopConfig.UUID,
				Name:        shopConfig.Name,
				Description: shopConfig.Description,
				Active:      true,
			}
			if err := db.Create(&shop).Error; err != nil {
				return err
			}
		} else {
			shop.Name = shopConfig.Name
			shop.Description = shopConfig.Description
			if err := db.Save(&shop).Error; err != nil {
				return err
			}
		}

		// Обновление терминалов
		for _, termConfig := range shopConfig.Terminals {
			var terminal Terminal
			result := db.Where("uuid = ?", termConfig.UUID).First(&terminal)
			if result.Error != nil && result.Error != gorm.ErrRecordNotFound {
				return result.Error
			}

			isNewTerminal := result.Error == gorm.ErrRecordNotFound
			if isNewTerminal {
				terminal = Terminal{
					UUID:           termConfig.UUID,
					Name:           termConfig.Name,
					TerminalKey:    termConfig.TerminalKey,
					SecretKey:      termConfig.SecretKey,
					NotificationURL: termConfig.NotificationURL,
					Active:         true,
					ShopID:         shop.ID,
				}
				if err := db.Create(&terminal).Error; err != nil {
					return err
				}
			} else {
				terminal.Name = termConfig.Name
				terminal.TerminalKey = termConfig.TerminalKey
				terminal.SecretKey = termConfig.SecretKey
				terminal.NotificationURL = termConfig.NotificationURL
				terminal.ShopID = shop.ID
				if err := db.Save(&terminal).Error; err != nil {
					return err
				}
			}
		}

		// Обновление продуктов
		for _, prodConfig := range shopConfig.Products {
			var product Product
			result := db.Where("uuid = ?", prodConfig.UUID).First(&product)
			if result.Error != nil && result.Error != gorm.ErrRecordNotFound {
				return result.Error
			}

			isNewProduct := result.Error == gorm.ErrRecordNotFound
			if isNewProduct {
				product = Product{
					UUID:        prodConfig.UUID,
					SKU:         prodConfig.SKU,
					Name:        prodConfig.Name,
					Description: prodConfig.Description,
					Price:       prodConfig.Price,
					Image:       prodConfig.Image,
					Active:      true,
					ShopID:      shop.ID,
				}
				if err := db.Create(&product).Error; err != nil {
					return err
				}
			} else {
				product.SKU = prodConfig.SKU
				product.Name = prodConfig.Name
				product.Description = prodConfig.Description
				product.Price = prodConfig.Price
				product.Image = prodConfig.Image
				product.ShopID = shop.ID
				if err := db.Save(&product).Error; err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// Обновление кэша магазинов
func updateShopCache() error {
	var shops []Shop
	if err := db.Preload("Terminals").Preload("Products").Where("active = ?", true).Find(&shops).Error; err != nil {
		return err
	}

	newCache := make(map[string]*Shop)
	for i := range shops {
		newCache[shops[i].UUID] = &shops[i]
	}

	shopsMutex.Lock()
	shopCache = newCache
	shopsMutex.Unlock()

	return nil
}

// Получение магазина из кэша
func getShopByUUID(uuid string) (*Shop, error) {
	shopsMutex.RLock()
	shop, exists := shopCache[uuid]
	shopsMutex.RUnlock()

	if !exists {
		return nil, fmt.Errorf("shop not found")
	}

	return shop, nil
}

// Выбор оптимального терминала для магазина
func selectTerminal(shop *Shop) (*Terminal, error) {
	if len(shop.Terminals) == 0 {
		return nil, fmt.Errorf("no active terminals for shop %s", shop.Name)
	}

	// Имитация балансировки нагрузки
	// В реальном приложении, можно использовать более сложную логику
	var activeTerminals []Terminal
	for _, term := range shop.Terminals {
		if term.Active {
			activeTerminals = append(activeTerminals, term)
		}
	}

	if len(activeTerminals) == 0 {
		return nil, fmt.Errorf("no active terminals for shop %s", shop.Name)
	}

	// Простой round-robin выбор терминала
	selected := activeTerminals[time.Now().UnixNano()%int64(len(activeTerminals))]
	return &selected, nil
}

// Проверка и подготовка заказа
func prepareOrder(req PaymentRequest) (*Shop, []Product, []OrderItem, uint64, error) {
	shop, err := getShopByUUID(req.ShopID)
	if err != nil {
		return nil, nil, nil, 0, err
	}

	if len(req.Products) == 0 {
		return nil, nil, nil, 0, fmt.Errorf("no products specified")
	}

	productMap := make(map[string]Product)
	for _, p := range shop.Products {
		if p.Active {
			productMap[p.UUID] = p
		}
	}

	var selectedProducts []Product
	var orderItems []OrderItem
	var totalAmount uint64

	for _, reqProduct := range req.Products {
		product, exists := productMap[reqProduct.ProductID]
		if !exists {
			return nil, nil, nil, 0, fmt.Errorf("product %s not found or inactive", reqProduct.ProductID)
		}

		if reqProduct.Quantity <= 0 {
			reqProduct.Quantity = 1
		}

		totalPrice := product.Price * uint64(reqProduct.Quantity)
		totalAmount += totalPrice

		selectedProducts = append(selectedProducts, product)
		orderItems = append(orderItems, OrderItem{
			UUID:       uuid.New().String(),
			ProductID:  product.ID,
			Product:    product,
			Quantity:   reqProduct.Quantity,
			Price:      product.Price,
			TotalPrice: totalPrice,
		})
	}

	if totalAmount == 0 {
		return nil, nil, nil, 0, fmt.Errorf("total amount cannot be zero")
	}

	return shop, selectedProducts, orderItems, totalAmount, nil
}

// Создание чека для Tinkoff API
func createReceipt(orderItems []OrderItem, email string) *tinkoff.Receipt {
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
		Taxation: "usn_income",
		Items:    items,
		Payments: &tinkoff.ReceiptPayments{
			Electronic: totalAmount,
		},
	}
}

// Генерация описания заказа
func generateDescription(orderItems []OrderItem) string {
	var names []string
	for _, item := range orderItems {
		if item.Quantity > 1 {
			names = append(names, fmt.Sprintf("%s x%d", item.Product.Name, item.Quantity))
		} else {
			names = append(names, item.Product.Name)
		}
	}
	return "Purchase: " + strings.Join(names, ", ")
}

// HTTP обработчики
func payHandler(tinkoffService *TinkoffService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req PaymentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Получить разрешение на выполнение в пуле воркеров
		ctx := r.Context()
		if err := sem.Acquire(ctx, 1); err != nil {
			http.Error(w, "Server is busy, try again later", http.StatusServiceUnavailable)
			return
		}
		defer sem.Release(1)

		// Проверяем и подготавливаем заказ
		shop, products, orderItems, totalAmount, err := prepareOrder(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Выбираем терминал
		terminal, err := selectTerminal(shop)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Используем или генерируем orderID
		orderID := req.OrderID
		if orderID == "" {
			orderID = uuid.New().String()
		}

		// Проверяем уникальность orderID
		var existingPayment Payment
		if db.Where("order_id = ?", orderID).First(&existingPayment).Error == nil {
			http.Error(w, "Order ID already exists", http.StatusBadRequest)
			return
		}

		// Формируем URL для успешной оплаты и неудачи
		var termConfig TerminalConfig
		for _, shopConfig := range config.Shops {
			if shopConfig.UUID == shop.UUID {
				for _, tc := range shopConfig.Terminals {
					if tc.UUID == terminal.UUID {
						termConfig = tc
						break
					}
				}
				break
			}
		}

		successURL := fmt.Sprintf("%s?order_id=%s", termConfig.SuccessURL, orderID)
		failURL := fmt.Sprintf("%s?order_id=%s", termConfig.FailURL, orderID)

		// Создаем запрос к Tinkoff
		tinkoffClient := tinkoffService.GetClient(terminal.TerminalKey, terminal.SecretKey)
		initReq := &tinkoff.InitRequest{
			Amount:          totalAmount,
			OrderID:         orderID,
			ClientIP:        req.ClientIP,
			Description:     generateDescription(orderItems),
			SuccessURL:      successURL,
			FailURL:         failURL,
			NotificationURL: terminal.NotificationURL,
			Receipt:         createReceipt(orderItems, req.Email),
		}

		// Создаем таймаут контекст для запроса
		reqCtx, cancel := context.WithTimeout(ctx, time.Duration(config.ExternalService.Timeout)*time.Second)
		defer cancel()

		// Инициализируем платеж
		initResp, err := tinkoffClient.InitWithContext(reqCtx, initReq)
		if err != nil {
			log.Printf("Payment init failed: %v", err)
			http.Error(w, "Payment initialization failed", http.StatusInternalServerError)
			return
		}

		// Создаем новый платеж в БД
		payment := Payment{
			UUID:       uuid.New().String(),
			OrderID:    orderID,
			PaymentID:  initResp.PaymentID,
			Amount:     totalAmount,
			Status:     "INIT",
			PlayerName: req.PlayerName,
			Email:      req.Email,
			ShopID:     shop.ID,
			Shop:       *shop,
			TerminalID: terminal.ID,
			Terminal:   *terminal,
			Items:      orderItems,
		}

		// Сохраняем платеж и его товары
		if err := db.Create(&payment).Error; err != nil {
			log.Printf("Failed to save payment: %v", err)
			http.Error(w, "Failed to process payment", http.StatusInternalServerError)
			return
		}

		// Возвращаем URL для оплаты
		json.NewEncoder(w).Encode(PaymentResponse{
			PaymentURL: initResp.PaymentURL,
			OrderID:    orderID,
		})
	}
}

func notifyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var notif PaymentNotificationData
		if err := json.NewDecoder(r.Body).Decode(&notif); err != nil {
			http.Error(w, "Invalid notification format", http.StatusBadRequest)
			return
		}

		// Проверка платежа в БД
		var payment Payment
		if err := db.Preload("Shop").Preload("Terminal").Preload("Items").Preload("Items.Product").Where("order_id = ?", notif.OrderID).First(&payment).Error; err != nil {
			log.Printf("Payment not found: %s", notif.OrderID)
			http.Error(w, "Payment not found", http.StatusBadRequest)
			return
		}

		// Обновляем статус платежа
		payment.Status = notif.Status
		if err := db.Save(&payment).Error; err != nil {
			log.Printf("Failed to update payment status: %v", err)
			http.Error(w, "Failed to update payment", http.StatusInternalServerError)
			return
		}

		// Если платеж подтвержден, отправляем информацию во внешний сервис
		if notif.Status == "CONFIRMED" {
			go processSuccessfulPayment(payment)
		}

		w.WriteHeader(http.StatusOK)
	}
}

func shopListHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		shopsMutex.RLock()
		shops := make([]ShopResponse, 0, len(shopCache))
		for _, shop := range shopCache {
			products := make([]ProductResponse, 0, len(shop.Products))
			for _, product := range shop.Products {
				if product.Active {
					products = append(products, ProductResponse{
						UUID:        product.UUID,
						SKU:         product.SKU,
						Name:        product.Name,
						Description: product.Description,
						Price:       product.Price,
						Image:       product.Image,
					})
				}
			}

			shops = append(shops, ShopResponse{
				UUID:        shop.UUID,
				Name:        shop.Name,
				Description: shop.Description,
				Products:    products,
			})
		}
		shopsMutex.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(shops)
	}
}

// Обработка успешного платежа
func processSuccessfulPayment(payment Payment) {
	log.Printf("Processing successful payment: %s", payment.OrderID)

	var products []map[string]interface{}
	for _, item := range payment.Items {
		products = append(products, map[string]interface{}{
			"id":       item.Product.UUID,
			"name":     item.Product.Name,
			"quantity": item.Quantity,
			"price":    item.Price,
			"total":    item.TotalPrice,
		})
	}

	payload := map[string]interface{}{
		"order_id":    payment.OrderID,
		"payment_id":  payment.PaymentID,
		"shop_id":     payment.Shop.UUID,
		"player_name": payment.PlayerName,
		"email":       payment.Email,
		"amount":      payment.Amount,
		"products":    products,
		"created_at":  payment.CreatedAt.Format(time.RFC3339),
	}

	// Повторяем отправку при ошибках
	for i := 0; i <= config.ExternalService.MaxRetries; i++ {
		if i > 0 {
			backoff := time.Duration(1<<uint(i-1)) * time.Second
			log.Printf("Retrying external service in %v (attempt %d/%d)", backoff, i, config.ExternalService.MaxRetries)
			time.Sleep(backoff)
		}

		if err := sendToExternalService(payload); err == nil {
			return
		}
	}

	log.Printf("Failed to send payment data to external service after %d attempts", config.ExternalService.MaxRetries+1)
}

// Отправка данных во внешний сервис
func sendToExternalService(payload map[string]interface{}) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Failed to marshal payload: %v", err)
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.ExternalService.Timeout)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", config.ExternalService.Domain, bytes.NewBuffer(payloadBytes))
	if err != nil {
		log.Printf("Failed to create request: %v", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("External service error: %v", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("External service returned error status: %s", resp.Status)
		return fmt.Errorf("external service error: %s", resp.Status)
	}

	log.Printf("External service response: %s", resp.Status)
	return nil
}

// Запуск периодического обновления кэша
func startCacheUpdater(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := updateShopCache(); err != nil {
				log.Printf("Failed to update shop cache: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// Проверка здоровья сервиса
func healthCheckHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		type HealthStatus struct {
			Status    string `json:"status"`
			Timestamp string `json:"timestamp"`
			Version   string `json:"version"`
		}

		status := HealthStatus{
			Status:    "ok",
			Timestamp: time.Now().Format(time.RFC3339),
			Version:   "2.0.0",
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	}
}

func main() {
	// Загрузка конфигурации
	if err := loadConfig(); err != nil {
		log.Fatalf("Config error: %v", err)
	}

	// Инициализация базы данных
	if err := initDB(); err != nil {
		log.Fatalf("Database error: %v", err)
	}

	// Загрузка магазинов из конфигурации
	if err := loadShops(); err != nil {
		log.Fatalf("Shop loading error: %v", err)
	}

	// Обновление кэша магазинов
	if err := updateShopCache(); err != nil {
		log.Fatalf("Shop cache update error: %v", err)
	}

	// Инициализация семафора для ограничения количества одновременных запросов
	sem = semaphore.NewWeighted(int64(config.Server.Workers))

	// Создание сервиса для работы с Tinkoff API
	tinkoffService := NewTinkoffService()

	// Настройка обработчиков маршрутов
	router := http.NewServeMux()
	router.HandleFunc("/pay", payHandler(tinkoffService))
	router.HandleFunc("/notify", notifyHandler())
	router.HandleFunc("/shops", shopListHandler())
	router.HandleFunc("/health", healthCheckHandler())

	// Настройка CORS
	corsHandler := cors.New(cors.Options{
		AllowedOrigins: config.CORS.AllowedOrigins,
		AllowedMethods: config.CORS.AllowedMethods,
		AllowedHeaders: []string{"Content-Type", "Authorization"},
	}).Handler(router)

	Addr:         ":" + config.Server.Port,
		Handler:      corsHandler,
		ReadTimeout:  time.Duration(config.Server.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(config.Server.WriteTimeout) * time.Second,
	}

	// Создание контекста для корректного завершения
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Запуск фонового обновления кэша
	go startCacheUpdater(ctx)

	// Настройка обработки сигналов для корректного завершения
	go gracefulShutdown(ctx, cancel, server)

	// Запуск HTTP сервера
	log.Printf("Server starting on port %s with %d workers", config.Server.Port, config.Server.Workers)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

// Корректное завершение работы сервера
func gracefulShutdown(ctx context.Context, cancel context.CancelFunc, server *http.Server) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	<-stop
	log.Println("Shutting down server...")
	cancel() // Отмена контекста для завершения работы всех goroutine

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}

	log.Println("Server gracefully stopped")
}
	// Создание HTTP сервера
	server := &http.Server{
		Addr:         ":" + config.Server.Port,
