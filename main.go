package main

import (
    "context"
    "crypto/tls"
    "encoding/json"
	"errors"
    "fmt"
	"os"
    "io"
    "log"
    "net"
    "net/http"
    "sync"
	"runtime"
    "time"

    "gopkg.in/yaml.v2"
	"github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/service/s3"
    "github.com/fatih/pool"
	"github.com/aws/smithy-go"

)

// Backend represents a backend server with connection pool and circuit breaker
type Backend struct {
    Address           string `yaml:"address" json:"address"`
    TLS               bool   `yaml:"tls" json:"tls"`
    InsecureSkipVerify bool   `yaml:"insecure_skip_verify" json:"insecure_skip_verify"`
    Connections       int
    ConnectionPool    pool.Pool
    Healthy           bool
    LastCheck         time.Time
    AvgLatency        time.Duration
    // Circuit breaker fields
    CircuitState    string
    FailureCount    int
    LastFailureTime time.Time
}

// CircuitBreakerConfig for setting up circuit breaker behavior
type CircuitBreakerConfig struct {
    MaxFailures     int           `yaml:"max_failures"`
    OpenTimeout     time.Duration `yaml:"open_timeout"`
    HalfOpenTimeout time.Duration `yaml:"half_open_timeout"`
}

type PoolConfig struct {
    InitialCap int `yaml:"initial_cap"`
    MaxCap     int `yaml:"max_cap"`
}
// Config holds the configuration for our proxy
type TCPConfig struct {
    ListenPort        string              `yaml:"listen_port"`
    CertFile          string              `yaml:"cert_file"`
    KeyFile           string              `yaml:"key_file"`
    Backends          []Backend           `yaml:"backends"`
    LoadBalancingType string              `yaml:"load_balancing_type"`
    PoolConfig        PoolConfig          `yaml:"pool_config"`
    HealthCheck       struct {
        Interval time.Duration `yaml:"interval"`
    } `yaml:"health_check"`
    KeepAlive struct {
        Interval time.Duration `yaml:"interval"`
    } `yaml:"keep_alive"`
    APIPort            string             `yaml:"api_port"`
    CircuitBreaker     CircuitBreakerConfig `yaml:"circuit_breaker"`
	ForceDisconnect    bool                `yaml:"force_disconnect"`
    S3Bucket           string              `yaml:"s3_bucket"`
    S3Key              string              `yaml:"s3_key"`
	mutex    		   sync.Mutex
}
var tcpconfig TCPConfig


// Proxy holds the state of our proxy including backends
type Proxy struct {
    backends []*Backend
    next     int
    mutex    sync.Mutex
}

var activeConnections = struct {
    sync.Mutex
    connections map[net.Conn]struct{}
}{
    connections: make(map[net.Conn]struct{}),
}

// NewProxy creates and returns a new Proxy
func NewProxy(config TCPConfig) *Proxy {
    var bs []*Backend
    for i := range config.Backends {
        b := &config.Backends[i]
        p, err := pool.NewChannelPool(config.PoolConfig.InitialCap, config.PoolConfig.MaxCap, func() (net.Conn, error) {
            var conn net.Conn
            var err error
            if b.TLS {
                tlsConfig := &tls.Config{InsecureSkipVerify: b.InsecureSkipVerify}
                conn, err = tls.Dial("tcp", b.Address, tlsConfig)
            } else {
                conn, err = net.Dial("tcp", b.Address)
            }
            if err != nil {
                return nil, err
            }
            return conn, nil
        })
        if err != nil {
            log.Fatalf("Failed to create pool for backend %s: %v", b.Address, err)
        }
        b.ConnectionPool = p
        b.Healthy = true // Initially assume healthy
        b.CircuitState = "Closed"
        bs = append(bs, b)
    }
    proxy := &Proxy{backends: bs, next: 0}
    go proxy.healthCheckLoop(tcpconfig.HealthCheck.Interval)
    return proxy
}

// readConfigFromS3 reads the configuration from an S3 bucket
func readConfigFromS3(ctx context.Context) ([]byte, error) {
    cfg, err := config.LoadDefaultConfig(ctx)
    if err != nil {
        return nil, fmt.Errorf("failed to load SDK config: %v", err)
    }

    client := s3.NewFromConfig(cfg)
    result, err := client.GetObject(ctx, &s3.GetObjectInput{
        Bucket: aws.String(tcpconfig.S3Bucket),
        Key:    aws.String(tcpconfig.S3Key),
    })
    if err != nil {
        return nil, fmt.Errorf("failed to get object: %v", err)
    }
    defer result.Body.Close()

    body, err := io.ReadAll(result.Body)
    if err != nil {
        return nil, fmt.Errorf("failed to read body: %v", err)
    }
    return body, nil
}

// writeConfigToS3 writes the configuration to an S3 bucket
func writeConfigToS3(ctx context.Context, file *os.File) error {
    cfg, err := config.LoadDefaultConfig(ctx)
    if err != nil {
        return fmt.Errorf("failed to load SDK config: %v", err)
    }

   s3client := s3.NewFromConfig(cfg)
    // Define the S3 bucket and key (file path in S3)
   bucket := localtcpconfig.S3Bucket
   key :=  localtcpconfig.S3Key
   defer file.Close()
	// Upload the file to S3
   _, err = s3client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   file,
	})
   if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "EntityTooLarge" {
			log.Printf("Error while uploading object to %s. The object is too large.\n"+
				"To upload objects larger than 5GB, use the S3 console (160GB max)\n"+
				"or the multipart upload API (5TB max).", bucket)
		} else {
			log.Printf("Couldn't upload file to %v:%v. Here's why: %v\n",
				 bucket, key, err)
		}
		log.Fatalf("Failed to upload file, %v", err)
   }else {
		err = s3.NewObjectExistsWaiter(s3client).Wait(
			ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}, time.Minute)
		if err != nil {
			log.Printf("Failed attempt to wait for object %s to exist.\n", key)
		}
	}
   fmt.Println("File uploaded successfully to S3.")
   return err
}

// loadConfig loads configuration from S3 instead of local file system
func loadConfig() {
    ctx := context.Background()
    content, err := readConfigFromS3(ctx)
    if err != nil {
        log.Fatalf("Error loading config from S3: %v", err)
    }
    err = yaml.Unmarshal(content, &tcpconfig)
    if err != nil {
        log.Fatalf("Error parsing YAML from S3: %v", err)
    }
}

// saveConfig saves the current configuration to S3 instead of local file system
func saveConfigS3(config TCPConfig) {
    yamlData, err := yaml.Marshal(config)
    if err != nil {
        log.Printf("Error marshalling config to YAML: %v", err)
        return
    }
	fileName := "./config.yaml"
	err1 := os.WriteFile("./config.yaml",yamlData,0644)
	if err1 != nil {
		log.Fatal(err1)
	}
	file, err2 := os.Open(fileName)
	if err2 != nil {
		log.Printf("Couldn't open file %v to upload. Here's why: %v\n", fileName, err2)
	} else {
		ctx := context.Background()
		if err3 := writeConfigToS3(ctx, file); err != nil {
			log.Printf("Error writing config to S3: %v", err3)
		}
	}
}

// healthCheckLoop periodically checks the health of backends
func (p *Proxy) healthCheckLoop(interval time.Duration) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    for range ticker.C {
        p.mutex.Lock()
        for _, backend := range p.backends {
            backend.Healthy = p.checkBackendHealth(backend)
            if backend.Healthy {
                // Measure latency if healthy
                latency, err := p.measureLatency(backend)
                if err == nil {
                    backend.AvgLatency = latency
                }
            }
            p.checkAndUpdateCircuitBreakerState(backend)
        }
        p.mutex.Unlock()
    }
}

// checkBackendHealth checks if a backend is healthy
func (p *Proxy) checkBackendHealth(backend *Backend) bool {
    conn, err := net.DialTimeout("tcp", backend.Address, 2*time.Second)
    if err != nil {
        log.Printf("Health check failed for %s: %v", backend.Address, err)
        return false
    }
    conn.Close()
    return true
}

// measureLatency measures the latency to a backend
func (p *Proxy) measureLatency(backend *Backend) (time.Duration, error) {
    start := time.Now()
    conn, err := net.DialTimeout("tcp", backend.Address, 2*time.Second)
    if err != nil {
        return 0, err
    }
    defer conn.Close()
    return time.Since(start), nil
}

// checkAndUpdateCircuitBreakerState updates the state of the circuit breaker
func (p *Proxy) checkAndUpdateCircuitBreakerState(backend *Backend) {
    switch backend.CircuitState {
    case "Closed":
        if backend.FailureCount >= tcpconfig.CircuitBreaker.MaxFailures {
            backend.CircuitState = "Open"
            backend.LastFailureTime = time.Now()
            log.Printf("Circuit breaker opened for %s", backend.Address)
        }
    case "Open":
        if time.Since(backend.LastFailureTime) > tcpconfig.CircuitBreaker.OpenTimeout {
            backend.CircuitState = "Half-Open"
            backend.FailureCount = 0
            log.Printf("Circuit breaker half-opened for %s", backend.Address)
        }
    case "Half-Open":
        // In this state, we should've already tried one request. If it succeeded, we'd close it; if failed, we'd go back to Open.
        if time.Since(backend.LastFailureTime) > tcpconfig.CircuitBreaker.HalfOpenTimeout {
            backend.CircuitState = "Closed"
            log.Printf("Circuit breaker closed for %s", backend.Address)
        }
    }
}

// nextBackend selects the next backend based on health, circuit breaker state, and load balancing strategy
func (p *Proxy) nextBackend(loadBalancingType string) *Backend {
    p.mutex.Lock()
    defer p.mutex.Unlock()

    healthyBackends := make([]*Backend, 0)
    for _, backend := range p.backends {
        if backend.Healthy && backend.CircuitState != "Open" {
            healthyBackends = append(healthyBackends, backend)
        }
    }

    if len(healthyBackends) == 0 {
        log.Println("No available backends - all are either unhealthy or circuit breakers are open")
        return nil
    }

    switch loadBalancingType {
    case "round_robin":
        backend := healthyBackends[p.next%len(healthyBackends)]
        p.next = (p.next + 1) % len(healthyBackends)
        return backend
    case "least_connections":
        var leastUsed *Backend
        leastConnections := int(^uint(0) >> 1) // Max int
        for _, backend := range healthyBackends {
            if backend.Connections < leastConnections {
                leastConnections = backend.Connections
                leastUsed = backend
            }
        }
        if leastUsed != nil {
            leastUsed.Connections++
        }
        return leastUsed
    default:
        log.Println("Unknown load balancing strategy, defaulting to round robin")
        return healthyBackends[p.next%len(healthyBackends)]
    }
}

// keepAlive keeps the connection alive by sending periodic pings
func keepAlive(conn net.Conn, interval time.Duration) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    for range ticker.C {
        _, err := conn.Write([]byte("\n")) // Send a newline as a keep-alive ping
        if err != nil {
            log.Printf("Keep-alive failed: %v", err)
            return // Connection is likely dead
        }
    }
}

// handleConnection manages a single client connection
func handleConnection(client net.Conn, backend *Backend, loadBalancingType string, keepAliveInterval time.Duration) {
    activeConnections.Lock()
    activeConnections.connections[client] = struct{}{}
    activeConnections.Unlock()
    defer func() {
        activeConnections.Lock()
        delete(activeConnections.connections, client)
        activeConnections.Unlock()
    }()

	if backend == nil {
        log.Println("No backend available for connection")
        client.Close()
        return
    }
    defer client.Close()

	

    conn, err := backend.ConnectionPool.Get()
    if err != nil {
        log.Printf("Error getting connection from pool for backend %s: %v", backend.Address, err)
        backend.FailureCount++
        if backend.CircuitState == "Half-Open" {
            backend.CircuitState = "Open"
        }
        return
    }
    defer func() {
        //backend.ConnectionPool.Put(conn)
        if loadBalancingType == "least_connections" {
            backend.Connections--
        }
    }()

    // Start keep-alive for this connection
    go keepAlive(conn, keepAliveInterval)

    // Bidirectional copy between client and server with error handling
    done := make(chan struct{})
    go func() {
        defer close(done)
        _, err := io.Copy(conn, client)
        if err != nil {
            log.Printf("Error copying from client to backend %s: %v", backend.Address, err)
            backend.FailureCount++
        }
    }()
    _, err = io.Copy(client, conn)
    if err != nil {
        log.Printf("Error copying from backend %s to client: %v", backend.Address, err)
        backend.FailureCount++
    } else {
        // If no error, and we're in Half-Open state, we can close the circuit
        if backend.CircuitState == "Half-Open" {
            backend.CircuitState = "Closed"
            backend.FailureCount = 0
        }
    }
    <-done
}

// addBackend adds a new backend to the config
func addBackend(proxy *Proxy, config *TCPConfig, newBackend Backend) {
    proxy.mutex.Lock()
    defer proxy.mutex.Unlock()

    // Check if the backend already exists
    for _, b := range config.Backends {
        if b.Address == newBackend.Address {
            log.Printf("Backend %s already exists", newBackend.Address)
            return
        }
    }

    // Add to the config
    config.Backends = append(config.Backends, newBackend)

    // Create a new pool for this backend
    p, err := pool.NewChannelPool(config.PoolConfig.InitialCap, config.PoolConfig.MaxCap, func() (net.Conn, error) {
        if newBackend.TLS {
            tlsConfig := &tls.Config{InsecureSkipVerify: newBackend.InsecureSkipVerify}
            return tls.Dial("tcp", newBackend.Address, tlsConfig)
        }
        return net.Dial("tcp", newBackend.Address)
    })
    if err != nil {
        log.Printf("Failed to create pool for new backend %s: %v", newBackend.Address, err)
        return
    }
    newBackend.ConnectionPool = p
    newBackend.Healthy = true // Assume healthy until checked
    proxy.backends = append(proxy.backends, &newBackend)

    // Persist changes to YAML
    saveConfig(*config)
}

// removeBackend removes a backend from the config
func removeBackend(proxy *Proxy, config *TCPConfig, address string) {
    proxy.mutex.Lock()
    defer proxy.mutex.Unlock()

    for i, b := range config.Backends {
        if b.Address == address {
            // Remove from config and proxy
            config.Backends = append(config.Backends[:i], config.Backends[i+1:]...)
            proxy.backends = append(proxy.backends[:i], proxy.backends[i+1:]...)
            // Close all connections in the pool for this backend
            b.ConnectionPool.Close()

            // Persist changes to YAML
            saveConfig(*config)
            return
        }
    }
    log.Printf("Backend %s not found", address)
}

// getBackends lists all backends
func getBackends(w http.ResponseWriter, config *TCPConfig) {
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(config.Backends)
}

// updateForceDisconnect updates the force_disconnect setting in config
func updateForceDisconnect(enabled bool) {
    tcpconfig.mutex.Lock()
    defer tcpconfig.mutex.Unlock()
    tcpconfig.ForceDisconnect = enabled
    saveConfig(tcpconfig)
	if enabled {
        log.Println("ForceDisconnect enabled, initiating client disconnections")
        go disconnectAllClients()
    } else {
        log.Println("ForceDisconnect disabled")
    }
}
// New function to disconnect all clients
func disconnectAllClients() {
    activeConnections.Lock()
    defer activeConnections.Unlock()

    for conn := range activeConnections.connections {
        log.Printf("Disconnecting client: %v", conn.RemoteAddr())
        // Close the connection to force disconnection
        conn.Close()
        delete(activeConnections.connections, conn)
    }
}


// saveConfig saves the current configuration to YAML file
func saveConfig(tcpconfig TCPConfig) {
	saveConfigS3(tcpconfig)
}

var localtcpconfig TCPConfig


func main() {
    // Read config file
    configFile, err := os.ReadFile("config.yaml")
    if err != nil {
        log.Fatalf("Error reading config file: %v", err)
    }
    err = yaml.Unmarshal(configFile, &localtcpconfig)
    if err != nil {
        log.Fatalf("Error parsing YAML: %v", err)
    }

	loadConfig()

    // Set up TLS for inbound TCP connections
    cert, err := tls.LoadX509KeyPair(tcpconfig.CertFile, tcpconfig.KeyFile)
    if err != nil {
        log.Fatalf("Server: loadkeys: %s", err)
    }
    tlsConfig := tls.Config{Certificates: []tls.Certificate{cert}}
    listener, err := tls.Listen("tcp", ":"+tcpconfig.ListenPort, &tlsConfig)
    if err != nil {
        log.Fatalf("Error listening: %v", err)
    }
    defer listener.Close()
	// Set GOMAXPROCS to utilize all CPU cores for better concurrency handling
	runtime.GOMAXPROCS(runtime.NumCPU())


    proxy := NewProxy(tcpconfig)
    log.Printf("Proxy started on port %s, distributing to %d backends with %s load balancing", tcpconfig.ListenPort, len(proxy.backends), tcpconfig.LoadBalancingType)

    // Start HTTP server for backend management API
	http.HandleFunc("/force_disconnect", func(w http.ResponseWriter, r *http.Request) {
        if r.Method == "POST" {
            var req struct {
                Enabled bool `json:"enabled"`
            }
            if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, err.Error(), http.StatusBadRequest)
                return
            }
            updateForceDisconnect(req.Enabled)
            w.WriteHeader(http.StatusOK)
            fmt.Fprintf(w, "Force disconnect %s", map[bool]string{true: "enabled", false: "disabled"}[req.Enabled])
        } else if r.Method == "GET" {
            w.Header().Set("Content-Type", "application/json")
            json.NewEncoder(w).Encode(map[string]bool{"force_disconnect": tcpconfig.ForceDisconnect})
        }
    })
   http.HandleFunc("/add", func(w http.ResponseWriter, r *http.Request) {
	var backend Backend
	if err := json.NewDecoder(r.Body).Decode(&backend); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	addBackend(proxy, &tcpconfig, backend)
	w.WriteHeader(http.StatusCreated)
	})

	http.HandleFunc("/remove", func(w http.ResponseWriter, r *http.Request) {
		address := r.URL.Query().Get("address")
		if address == "" {
			http.Error(w, "Address parameter missing", http.StatusBadRequest)
			return
		}
		removeBackend(proxy, &tcpconfig, address)
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/get", func(w http.ResponseWriter, r *http.Request) {
		getBackends(w, &tcpconfig)
	})

    go func() {
        log.Printf("Starting API on port %s", tcpconfig.APIPort)
        if err := http.ListenAndServe(":"+tcpconfig.APIPort, nil); err != nil {
            log.Fatalf("HTTP server error: %v", err)
        }
    }()

    for {
        client, err := listener.Accept()
        if err != nil {
            log.Printf("Error accepting connection: %v", err)
            continue
        }
        go func(client net.Conn) {
            defer client.Close()
            for {
                if tcpconfig.ForceDisconnect {
                    log.Println("Forcing client disconnect due to configuration")
                    return
                }
                backend := proxy.nextBackend(tcpconfig.LoadBalancingType)
                if backend == nil {
                    log.Println("No backend available for connection")
                    return
                }
                handleConnection(client, backend, tcpconfig.LoadBalancingType, tcpconfig.KeepAlive.Interval)
                // If the connection handling finishes, it means either the client or backend closed the connection
                // or an error occurred. We'll allow for reconnection unless force disconnect is enabled.
            }
        }(client)
    }
}