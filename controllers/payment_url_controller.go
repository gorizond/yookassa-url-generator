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
	"github.com/rvinnie/yookassa-sdk-go/yookassa"
	yoocommon "github.com/rvinnie/yookassa-sdk-go/yookassa/common"
	yoopayment "github.com/rvinnie/yookassa-sdk-go/yookassa/payment"
	yoowebhook "github.com/rvinnie/yookassa-sdk-go/yookassa/webhook"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/dynamic"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	controller := &PaymentURLController{
		PaymentHandler: paymentHandler,
		BaseURL:        baseURL,
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
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Amount    string `json:"amount"`
	}

	var req PaymentRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
	    // Попробовать получить из формы
	    req.Namespace = ctx.PostForm("namespace")
	    req.Name = ctx.PostForm("name")
	    req.Amount = ctx.PostForm("amount")
	    if req.Namespace == "" || req.Name == "" || req.Amount == "" {
	        ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
	        return
	    }
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
			ReturnURL: fmt.Sprintf("%s/dashboard/c/_/gorizond/provisioning.gorizond.io.billing", c.BaseURL, req.Namespace, req.Name),
		},
		Description: fmt.Sprintf("Payment for %s/%s", req.Namespace, req.Name),
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
		fmt.Sscanf(paymentData.Description, "Payment for %s/%s", &namespace, &billing)

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

	setting, err := dynClient.Resource(gvr).Get(ctx, "gorizond-install-payment-url", metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	// value хранится в .Object["value"]
	if val, ok := setting.Object["value"].(string); ok {
		return val, nil
	}
	return "", fmt.Errorf("value not found in Setting")
}