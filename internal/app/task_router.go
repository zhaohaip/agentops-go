package app

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/zhaohaip/agentops-go/internal/api"
)

// NewTaskRouter 在应用组合根中创建 Task Handler 并集中注册 Gin 路由。
func NewTaskRouter(dependencies api.TaskHandlerDependencies) (*gin.Engine, error) {
	handler, err := api.NewTaskHandler(dependencies)
	if err != nil {
		return nil, fmt.Errorf("create Task API router: %w", err)
	}

	router := gin.New()
	router.HandleMethodNotAllowed = true
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.Use(handler.LogRequest, handler.Authenticate)
	router.NoRoute(func(ginContext *gin.Context) {
		writeRouteNotFound(ginContext)
	})
	router.NoMethod(func(ginContext *gin.Context) {
		writeRouteNotFound(ginContext)
	})

	tasks := router.Group("/v1/tasks")
	tasks.POST("", handler.Create)
	tasks.GET("", handler.List)
	tasks.GET("/:task_id", handler.Get)
	tasks.POST("/:task_id/cancel", handler.Cancel)
	return router, nil
}

type routeErrorResponse struct {
	Error routeErrorDetails `json:"error"`
}

type routeErrorDetails struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeRouteNotFound(ginContext *gin.Context) {
	ginContext.Header("Content-Type", "application/json")
	ginContext.JSON(http.StatusNotFound, routeErrorResponse{
		Error: routeErrorDetails{Code: "NotFound", Message: "resource not found"},
	})
}
