# TCP Proxy with Load Balancing, Circuit Breaking, and Dynamic Management

This Go application implements a TCP proxy with advanced features like load balancing, connection pooling, circuit breaking, and a management API. Here's what it does in simple terms:

## Key Features Explained

### **1. TCP Proxy**
- **What it does:** Acts like a middleman between clients and backend servers. Clients connect to the proxy, and the proxy forwards their requests to the appropriate backend server.

### **2. Load Balancing**
- **What it does:** Distributes incoming client connections across multiple backend servers to ensure no single server gets overwhelmed.
  - **Round Robin:** Takes turns sending connections to each backend in a circular order.
  - **Least Connections:** Sends new connections to the backend with the fewest active connections.

### **3. Connection Pooling**
- **What it does:** Keeps a pool of open connections to backends. Instead of opening a new connection for each client request, it reuses existing ones, which can be faster.

### **4. Circuit Breaker**
- **What it does:** If a backend server starts failing (like not responding), the circuit breaker mechanism temporarily stops sending requests to it, giving it time to recover. This helps prevent one bad server from affecting all traffic.

### **5. Health Checks**
- **What it does:** Regularly checks if backend servers are up and running. If a server is down, it won't get new connections until it's healthy again.

### **6. Dynamic Backend Management**
- **What it does:** 
  - **Add Backends:** You can add new backend servers on-the-fly without restarting the proxy.
  - **Remove Backends:** Similarly, you can remove servers when they're no longer needed.
  - **Get Backends:** Check which backends are currently in use.

### **7. Force Disconnect**
- **What it does:** Allows you to force all existing client connections to disconnect, which is useful for maintenance or emergency situations.

### **8. Keep-Alive**
- **What it does:** For persistent connections, it sends periodic "pings" to keep connections alive, preventing them from timing out due to inactivity.

## How to Use

### Setup
1. **Configuration:**
   - Edit `config.yaml` to set up your proxy's listen port, backend servers, load balancing strategy, etc.
   - Ensure you have SSL certificates (`server.crt` and `server.key`) for TLS connections.

2. **Build and Run:**
   - Install necessary libraries:
     ```bash
     go get github.com/fatih/pool
     ```
   - Build the proxy:
     ```bash
     go build main.go
     ```
   - Run the proxy:
     ```bash
     ./main
     ```

### API Endpoints
- **Add a Backend:** 
  - `POST /add` with a JSON body like `{"address": "127.0.0.1:8083", "tls": false, "insecure_skip_verify": false}`
- **Remove a Backend:**
  - `GET /remove?address=127.0.0.1:8083`
- **List Backends:**
  - `GET /get`
- **Enable/Disable Force Disconnect:**
  - `POST /force_disconnect` with `{"enabled": true}` or `{"enabled": false}`
  - `GET /force_disconnect` to check status

### Layman's Explanation

- Imagine you have a busy restaurant (the backend servers) where you (the proxy) are the maître d'. 
  - **Load Balancing:** You ensure that no single waiter gets all the tables by evenly distributing customers.
  - **Connection Pooling:** Instead of seating new customers at a new table each time, you guide them to tables that are just cleaned and ready.
  - **Circuit Breaker:** If one waiter starts dropping plates, you don't send more customers their way until they're back on their game.
  - **Health Checks:** You check if each waiter is ready to serve before seating anyone at their tables.
  - **Dynamic Management:** If a new waiter joins or one leaves, you can adjust the seating plan right away.
  - **Force Disconnect:** Sometimes, if you need to close the restaurant quickly (like for a fire drill), you can ask all customers to leave at once.
  - **Keep-Alive:** You occasionally check in with seated customers to make sure they're still there, so their table doesn't get reassigned too soon.

This proxy makes managing network traffic efficient, reliable, and adaptable to changes without downtime.

To modify the TCP proxy to support more than 10,000 connections, we need to consider several aspects:

- Increasing File Descriptors: On Linux systems, the number of simultaneous connections is often limited by the number of available file descriptors. We need to increase this limit.
- Adjusting System Settings: Modify system parameters like net.core.somaxconn to handle more pending connections.
- Optimizing Go Runtime: Ensure Go's runtime can handle a high number of goroutines by potentially adjusting GOMAXPROCS.
- Resource Management: Efficiently manage resources like memory and CPU to handle the increased load.