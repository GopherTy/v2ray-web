package v2ray

import (
	"bufio"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"os"

	"github.com/gopherty/broadcaster"

	"github.com/gopherty/v2rayW/logger"
	"github.com/gopherty/v2rayW/model"
	"github.com/gopherty/v2rayW/serve"
	"github.com/gopherty/v2rayW/token"
	"github.com/gopherty/v2rayW/utils"

	"github.com/gin-gonic/gin/binding"
	"github.com/gorilla/websocket"

	// v2ray core 启动的依赖
	_ "v2ray.com/core/app/dispatcher"
	_ "v2ray.com/core/app/proxyman/inbound"
	_ "v2ray.com/core/app/proxyman/outbound"

	"v2ray.com/core"

	"github.com/gin-gonic/gin"
)

// 启动成功后保存实例的状态用于关闭
var (
	instance *core.Instance
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	// v2ray 服务状态, true 代表运行，false 代表停止。
	stat = &Status{}
	// 用于发送服务器状态的通道
	statCh = make(chan *Status, 1)

	// 日志输出相关变量
	r, w, stdout *os.File
	bc           broadcaster.Broadcaster
	started      bool
)

// Dispatcher v2ray 功能相关的控制器
type Dispatcher struct {
}

// Start 启动 v2ray 服务
func (Dispatcher) Start(c *gin.Context) {
	// 将前端传过来的参数解析成 JSON 格式写入到文件中。
	protocol, id, err := parmasToJSON(c)
	if err != nil {
		logger.Logger().Error(err.Error())
		c.JSON(http.StatusInternalServerError, model.BackToFrontEndData{
			Code:        serve.StatusParamNotMatched,
			Description: "解析参数错误",
			Error:       err.Error(),
		})
		return
	}

	// 暂定 v2ray 配置文件名称和位置硬编码，因为是通过 web-ui 来对配置文件进行操作。
	path := utils.BasePath() + "/v2ray.json"
	file, err := os.Open(path)
	if err != nil {
		logger.Logger().Error(err.Error())
		c.JSON(http.StatusInternalServerError, model.BackToFrontEndData{
			Code:        serve.StatusServerError,
			Description: "打开 v2ray 配置文件出错",
			Error:       err.Error(),
		})
		return
	}
	defer file.Close()

	// 需要 json 格式注册解析器，默认为 protobuf。
	// 注册配置文件加载器
	// err = core.RegisterConfigLoader(&core.ConfigFormat{
	// 	Name:      "JSON",
	// 	Extension: []string{"json"},
	// 	Loader:    loaderJSON,
	// })
	// if err != nil {
	// 	logger.Logger().Error(err.Error())
	// 	c.JSON(http.StatusInternalServerError, model.BackToFrontEndData{
	// 		Code:        serve.StatusServerError,
	// 		Description: "加载 v2ray 配置文件出错",
	// 		Error:       err.Error(),
	// 	})
	// 	return
	// }

	// 解析 v2ray 的配置文件
	config, err := core.LoadConfig("json", path, file)
	if err != nil {
		logger.Logger().Error(err.Error())
		c.JSON(http.StatusInternalServerError, model.BackToFrontEndData{
			Code:        serve.StatusServerError,
			Description: "加载 v2ray 配置文件出错",
			Error:       err.Error(),
		})
		return
	}

	// 创建 v2ray 实例
	instance, err = core.New(config)
	if err != nil {
		logger.Logger().Error(err.Error())
		c.JSON(http.StatusInternalServerError, model.BackToFrontEndData{
			Code:        serve.StatusV2rayError,
			Description: "创建 v2ray 实例出错",
			Error:       err.Error(),
		})
		return
	}

	// 在 v2ray 启动之前捕获控制台输出。
	stdout = os.Stdout
	r, w, err = os.Pipe()
	if err != nil {
		os.Stdout = stdout
		stdout = nil
		w.Close()
		bc.Close()
		logger.Logger().Error(err.Error())
		return
	}
	os.Stdout = w
	reader := bufio.NewReader(r)

	// 启动 v2ray
	err = instance.Start()
	if err != nil {
		os.Stdout = stdout
		stdout = nil
		w.Close()
		bc.Close()
		logger.Logger().Error(err.Error())
		c.JSON(http.StatusInternalServerError, model.BackToFrontEndData{
			Code:        serve.StatusV2rayError,
			Description: "v2ray 启动错误",
			Error:       err.Error(),
		})
		return
	}

	// 发送日志
	go func() {
		for {
			logs, err := reader.ReadBytes('\n')
			if err != nil {
				logger.Logger().Error(err.Error())
				break
			}
			err = bc.Publish(logs)
			if err != nil {
				logger.Logger().Error(err.Error())
				break
			}
		}
	}()

	// 保存 v2ray 的状态
	stat.mu.Lock()
	stat.id = id
	stat.protocol = protocol
	stat.running = true
	stat.mu.Unlock()

	// 推送 v2ray 状态消息
	statCh <- stat

	c.JSON(http.StatusOK, model.BackToFrontEndData{
		Code: serve.StatusOK,
		Data: map[string]interface{}{
			"msg": "服务启动成功",
		},
	})
}

// Stop 关闭 v2ray 服务
func (Dispatcher) Stop(c *gin.Context) {
	// 归还控制台
	defer func() {
		os.Stdout = stdout
		stdout = nil
		w.Close()
	}()

	// 关闭广播
	defer func() {
		logger.Logger().Sugar().Info("-------- stop bc ........", started)
		if started {
			started = false
			err := bc.Close()
			if err != nil {
				logger.Logger().Sugar().Info("repete", err)
			}
		}
	}()

	if instance == nil {
		c.JSON(http.StatusInternalServerError, model.BackToFrontEndData{
			Code:  serve.StatusV2rayError,
			Error: "服务未启动",
		})
		return
	}

	err := instance.Close()
	if err != nil {
		logger.Logger().Error(err.Error())
		c.JSON(http.StatusInternalServerError, model.BackToFrontEndData{
			Code:        serve.StatusV2rayError,
			Description: "v2ray 关闭服务失败",
			Error:       err.Error(),
		})
		return
	}

	// 保存 v2ray 的状态
	stat.mu.Lock()
	stat.running = false
	stat.mu.Unlock()

	// 推送 v2ray 状态消息
	statCh <- stat

	c.JSON(http.StatusOK, model.BackToFrontEndData{
		Code: serve.StatusOK,
		Data: map[string]interface{}{
			"msg": "服务关闭成功",
		},
	})
}

// Logs v2ray 启动后日志输出接口
func (Dispatcher) Logs(c *gin.Context) {
	// 将 token 加入到 websocket 的子协议中，不设置 chrome 浏览器，不能使用 websocket。
	upgrader.Subprotocols = []string{c.GetHeader("Sec-WebSocket-Protocol")}
	// 获取日志输出
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		logger.Logger().Error(err.Error())
		return
	}
	defer conn.Close()

	// 先将 http 请求升级为 websocket ，在验证 token 是否过期，如果过期则返回 websocket 中的保留状态码 5000-10000
	// 客户端知道 token 过期，就将刷新 token 或 重新登录后再次请求该接口。
	err = token.ValidWSToken(c.Request)
	if err != nil {
		logger.Logger().Error(err.Error())

		closed := conn.CloseHandler()
		err = closed(5001, "unauthrizationed")
		if err != nil {
			logger.Logger().Error(err.Error())
		}
		return
	}

	// 开启广播器
	logger.Logger().Sugar().Info("-------- start bc ........", started)
	if !started {
		bc = broadcaster.NewBroadcaster(0)
		go bc.Run()
		started = true
	}

	// 增加订阅
	logs := make(chan interface{})
	bc.Subscribe(logs)

	go func() {
		for {
			select {
			case log := <-logs:
				if v, ok := log.([]byte); ok {
					err = conn.WriteMessage(websocket.TextMessage, v)
					if err != nil {
						if websocket.IsCloseError(err, websocket.CloseGoingAway) {
							logger.Logger().Warn(err.Error())
						} else {
							logger.Logger().Error(err.Error())
						}
						return
					}
				} else {
					return
				}
			case <-bc.Done():
				return
			}
		}
	}()

	// 读取客户端发送的停止日志输出。
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			// 取消订阅
			close(logs)
			bc.Unsubscribe(logs)
			if websocket.IsCloseError(err, websocket.CloseGoingAway) {
				logger.Logger().Warn(err.Error())
			} else {
				logger.Logger().Error(err.Error())
			}
			break
		}
	}
}

// Status 服务器端 v2ray 的状态
func (Dispatcher) Status(c *gin.Context) {
	// 将 token 加入到 websocket 的子协议中，不设置 chrome 浏览器，不能使用 websocket。
	upgrader.Subprotocols = []string{c.GetHeader("Sec-WebSocket-Protocol")}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		logger.Logger().Error(err.Error())
		return
	}
	defer conn.Close()

	// 先将 http 请求升级为 websocket ，在验证 token 是否过期，如果过期则返回 websocket 中的保留状态码 5000-10000
	// 客户端知道 token 过期，就将刷新 token 或 重新登录后再次请求该接口。
	err = token.ValidWSToken(c.Request)
	if err != nil {
		logger.Logger().Error(err.Error())
		closed := conn.CloseHandler()
		err = closed(5001, "unauthrizationed")
		if err != nil {
			logger.Logger().Error(err.Error())
		}
		return
	}

	// 先发送一次之前的状态
	err = conn.WriteJSON(gin.H{
		"running":  stat.running,
		"protocol": stat.protocol,
		"id":       stat.id,
	})
	if err != nil {
		if websocket.IsCloseError(err, websocket.CloseGoingAway) {
			logger.Logger().Warn(err.Error())
		} else {
			logger.Logger().Error(err.Error())
		}
		return
	}

	// 信号量，用于关闭发送消息协程。
	signal := make(chan int)
	// 发送服务器端 v2ray 的状态
	go func() {
		for {
			select {
			case status := <-statCh:
				err := conn.WriteJSON(gin.H{
					"running":  status.running,
					"protocol": status.protocol,
					"id":       status.id,
				})
				if err != nil {
					if websocket.IsCloseError(err, websocket.CloseGoingAway) {
						logger.Logger().Warn(err.Error())
					} else {
						logger.Logger().Error(err.Error())
					}
					return
				}
			case <-signal:
				return
			}
		}
	}()

	// 读取客户端发送的关闭连接。
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			signal <- 1 // 发送关闭信号
			if websocket.IsCloseError(err, websocket.CloseGoingAway) {
				logger.Logger().Warn(err.Error())
			} else {
				logger.Logger().Error(err.Error())
			}
			break
		}
	}
}

// parmasToJSON 将 v2ray 启动参数转化为配置文件
func parmasToJSON(c *gin.Context) (protocol string, id int, err error) {
	// 绑定参数
	var param ParamStart
	err = c.ShouldBindWith(&param, binding.Default(c.Request.Method, c.ContentType()))
	if err != nil {
		return
	}

	// 入口协议
	destOverride := []string{"http", "tls"}
	sock := Inbound{
		Port:     1080,
		Listen:   "127.0.0.1",
		Protocol: "socks",
		Settings: Socks{
			Auth: "noauth",
		},
		Sniffing: Sniffing{
			Enabled:      true,
			DestOverride: destOverride,
		},
	}
	inbounds := []Inbound{sock}

	user := User{
		ID:       param.UserID,
		AlterID:  param.AlertID,
		Level:    param.Level,
		Security: param.Security,
	}
	users := []User{user}

	vmess := Vmess{
		Address: param.Address,
		Port:    param.Port,
		Users:   users,
	}
	protocols := []interface{}{vmess}

	outboundConf := OutboundConfiguration{
		Vnext: protocols,
	}

	outbound := Outbound{
		Protocol: "vmess",
		Settings: outboundConf,
		StreamSettings: StreamSettings{
			NetWork:  param.Network,
			Security: param.NetSecurity,
			WSSettings: WebSocket{
				Path: param.Path,
			},
		},
		Mux: Mux{
			Enabled: true,
		},
	}

	outbounds := []Outbound{outbound}
	cnf := Config{
		Inbounds:  inbounds,
		Outbounds: outbounds,
	}

	str, err := json.MarshalIndent(&cnf, "", "    ")
	if err != nil {
		return
	}

	path := utils.BasePath() + "/v2ray.json"
	err = ioutil.WriteFile(path, str, 0666)
	if err != nil {
		return
	}

	return param.Protocol, param.ID, err
}
