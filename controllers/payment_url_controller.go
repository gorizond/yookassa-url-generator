package controllers

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	provisioningv1 "github.com/gorizond/gorizond-cluster/pkg/apis/provisioning.gorizond.io/v1"
	provisioningv1Controller "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io/v1"
	controllersGorizond "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io"
	"github.com/rvinnie/yookassa-sdk-go/yookassa"
	yoocommon "github.com/rvinnie/yookassa-sdk-go/yookassa/common"
	yoopayment "github.com/rvinnie/yookassa-sdk-go/yookassa/payment"
	yoowebhook "github.com/rvinnie/yookassa-sdk-go/yookassa/webhook"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

type PaymentURLController struct {
	PaymentHandler         *yookassa.PaymentHandler
	BaseURL                string
	BillingEventController provisioningv1Controller.BillingEventController
}

// инициализируем клиента и обработчик платежей
func InitPaymentURLController(ctx context.Context, config *rest.Config) {
	shopID := os.Getenv("YOOKASSA_SHOP_ID")
	secretKey := os.Getenv("YOOKASSA_SECRET_KEY")

	client := yookassa.NewClient(shopID, secretKey)
	paymentHandler := yookassa.NewPaymentHandler(client)
	
	baseURL, err := GetPaymentBaseURL(ctx, config)
	if err != nil {
		panic(fmt.Sprintf("failed to get payment base url: %v", err))
	}
	// 1) Создаём factory для generated контроллеров
    factory, err := controllersGorizond.NewFactoryFromConfig(config)
    if err != nil {
        panic(fmt.Errorf("failed to build controller factory: %w", err))
    }

    // 2) Получаем контроллер нужного ресурса
    beController := factory.Provisioning().V1().BillingEvent()
    // 3) Запускаем factory и ждём синхронизацию кэшей
    go func() {
        if err := factory.Start(ctx, 2 /* threads */); err != nil {
            panic(fmt.Errorf("factory start failed: %w", err))
        }
    }()
    if err := factory.Sync(ctx); err != nil {
        panic(fmt.Errorf("factory sync failed: %w", err))
    }
    
	controller := &PaymentURLController{
		PaymentHandler: paymentHandler,
		BaseURL:        baseURL,
		BillingEventController: beController,
	}

	router := gin.Default()
	router.POST("/payment", controller.GeneratePaymentURL)
	router.POST("/yookassa/webhook", controller.HandleWebhook)
	router.GET("/", func(ctx *gin.Context) {
	    ctx.Status(http.StatusOK)
	})
	go router.Run(":80")
}

func (c *PaymentURLController) GeneratePaymentURL(ctx *gin.Context) {
	// Структура для парсинга входящего JSON
	type PaymentRequest struct {
	    Namespace string `json:"namespace" form:"namespace"`
	    Name      string `json:"name"      form:"name"`
	    Amount    string `json:"amount"    form:"amount"`
	}

	var req PaymentRequest
	// Один вызов — Gin сам выберет биндер по Content-Type (JSON, form, multipart)
	if err := ctx.ShouldBind(&req); err != nil || req.Namespace == "" || req.Name == "" || req.Amount == "" {
	    ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
	    return
	}

	// формируем запрос на создание платежа
	paymentReq := &yoopayment.Payment{
		Amount: &yoocommon.Amount{
			Value:    req.Amount,
			Currency: "RUB",
		},
		PaymentMethod: yoopayment.PaymentMethodType("bank_card"),
		Confirmation: yoopayment.Redirect{
			Type: yoopayment.TypeRedirect,
			ReturnURL: fmt.Sprintf("%s/dashboard/c/_/gorizond/provisioning.gorizond.io.billing", c.BaseURL),
		},
		Description: fmt.Sprintf("Payment for %s/%s", req.Namespace, req.Name),
		Metadata: map[string]string{ // <-- новое
	        "namespace": req.Namespace,
	        "billing":   req.Name,
		},
	}

	// создаём платёж и получаем ссылку для оплаты
	link, err := c.PaymentHandler.CreatePaymentLink(paymentReq)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	ctx.Redirect(http.StatusMovedPermanently, link)
}

func (c *PaymentURLController) HandleWebhook(ctx *gin.Context) {
	var event yoowebhook.WebhookEvent[yoopayment.Payment]
	if err := ctx.ShouldBindJSON(&event); err != nil {
		ctx.Status(http.StatusBadRequest)
		return
	}


	// интересует только событие успешной оплаты
	if event.Event == "payment.succeeded" {
		paymentData := event.Object
		// Подтверждаем по API ЮKassa (source of truth)
		pay, err := c.PaymentHandler.FindPayment(paymentData.ID)
		if err != nil || pay == nil || pay.Status != yoopayment.Succeeded {
			ctx.Status(http.StatusAccepted) // примем, но не обрабатываем
			return
		}
		// извлекаем namespace и billing из описания платежа
		var namespace, billing string
		// 1) Пытаемся из metadata (надёжно)
		if m, ok := paymentData.Metadata.(map[string]interface{}); ok {
		    if v, ok := m["namespace"].(string); ok { namespace = v }
		    if v, ok := m["billing"].(string); ok { billing = v }
		}
		
		// 2) Фолбэк на description (если вдруг metadata пустое)
		if namespace == "" || billing == "" {
		    // "Payment for <ns>/<billing>", где <ns> может содержать '-'
		    fmt.Sscanf(paymentData.Description, "Payment for %[^/]/%s", &namespace, &billing)
		    // %[^/] читает до первого '/', затем %s — остальное (без пробелов)
		}
		floatValue, err := strconv.ParseFloat(paymentData.Amount.Value, 64)
		if err != nil {
			fmt.Println("Error parse ParseFloat:", err)
			return
		}
		// формируем и создаём BillingEvent
		be := &provisioningv1.BillingEvent{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "payment-",
				Namespace:    namespace,
			},
			Status: provisioningv1.BillingEventStatus{
				Type:           "payment",
				TransitionTime: metav1.NewTime(time.Now()),
				Amount:         floatValue,
				BillingName:    billing,
			},
		}
		if _, err := c.BillingEventController.Create(be); err != nil {
			// логируем ошибку, но всегда отвечаем 200 OK, чтобы YooKassa не повторяла уведомление
			fmt.Printf("error creating BillingEvent: %v\n", err)
		}
	}

	// подтверждаем получение webhook
	ctx.Status(http.StatusOK)
}

// Получить значение Setting из Rancher API
func GetPaymentBaseURL(ctx context.Context, config *rest.Config) (string, error) {
	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return "", err
	}

	gvr := schema.GroupVersionResource{
		Group:    "management.cattle.io",
		Version:  "v3",
		Resource: "settings",
	}

	setting, err := dynClient.Resource(gvr).Get(ctx, "server-url", metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	// value хранится в .Object["value"]
	if val, ok := setting.Object["value"].(string); ok {
		return val, nil
	}
	return "", fmt.Errorf("value not found in Setting")
}