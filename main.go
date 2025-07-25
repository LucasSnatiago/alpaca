// Copyright 2019, 2021, 2022 The Alpaca Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
)

var BuildVersion string

func whoAmI() string {
	me, err := user.Current()
	if err != nil {
		return ""
	}
	return me.Username
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile | log.Lmicroseconds)
	host := flag.String("l", "localhost", "address to listen on")
	port := flag.Int("p", 3128, "http port number to listen on")
	socksPort := flag.Int("s", 8010, "socks port number to listen on")
	pacurl := flag.String("C", "", "url of proxy auto-config (pac) file")
	domain := flag.String("d", "", "domain of the proxy account (for NTLM auth)")
	username := flag.String("u", whoAmI(), "username of the proxy account (for NTLM auth)")
	printHash := flag.Bool("H", false, "print hashed NTLM credentials for non-interactive use")
	version := flag.Bool("version", false, "print version number")
	flag.Parse()

	if *version {
		fmt.Println("Alpaca", BuildVersion)
		os.Exit(0)
	}

	var src credentialSource
	if *domain != "" {
		src = fromTerminal().forUser(*domain, *username)
	} else if value := os.Getenv("NTLM_CREDENTIALS"); value != "" {
		src = fromEnvVar(value)
	} else {
		src = fromKeyring()
	}

	var a *authenticator
	if src != nil {
		var err error
		a, err = src.getCredentials()
		if err != nil {
			log.Printf("Credentials not found, disabling proxy auth: %v", err)
		}
	}

	if *printHash {
		if a == nil {
			fmt.Println("Please specify a domain (using -d) and username (using -u)")
			os.Exit(1)
		}
		fmt.Printf("# Add this to your ~/.profile (or equivalent) and restart your shell\n")
		fmt.Printf("NTLM_CREDENTIALS=%q; export NTLM_CREDENTIALS\n", a)
		os.Exit(0)
	}

	errch := make(chan error)

	// http server
	s := createServer(*host, *port, *pacurl, a)

	for _, network := range networks(*host) {
		// HTTP/HTTPS Server
		go func(network string) {
			l, err := net.Listen(network, ":"+strconv.Itoa(*port))
			if err != nil {
				errch <- err
			} else {
				log.Printf("Listening on %s %s", network, s.Addr)
				errch <- s.Serve(l)
			}
		}(network)

		// Socks5 server
		go func(network string) {
			socksaddr := fmt.Sprintf("%s:%d", *host, *socksPort)
			httpaddr := fmt.Sprintf("%s:%d", *host, *port)
			srv, err := startSocksServer(httpaddr, a)
			if err != nil {
				log.Printf("Failed to start socks5 server: %v", err)
			} else {
				log.Printf("SOCKS5 (via HTTP proxy %s) listening on %s", httpaddr, socksaddr)
				errch <- srv.ListenAndServe(network, socksaddr)
			}
		}(network)
	}

	log.Fatal(<-errch)
}

func createServer(host string, port int, pacurl string, a *authenticator) *http.Server {
	pacWrapper := NewPACWrapper(PACData{Port: port})
	proxyFinder := NewProxyFinder(pacurl, pacWrapper)
	proxyHandler := NewProxyHandler(a, getProxyFromContext, proxyFinder.blockProxy)
	mux := http.NewServeMux()
	pacWrapper.SetupHandlers(mux)

	// build the handler by wrapping middleware upon middleware
	var handler http.Handler = mux
	handler = RequestLogger(handler)
	handler = proxyHandler.WrapHandler(handler)
	handler = proxyFinder.WrapHandler(handler)
	handler = AddContextID(handler)

	return &http.Server{
		// Set the addr to host(defaults to localhost) : port(defaults to 3128)
		Addr:    net.JoinHostPort(host, strconv.Itoa(port)),
		Handler: handler,
		// TODO: Implement HTTP/2 support. In the meantime, set TLSNextProto to a non-nil
		// value to disable HTTP/2.
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}
}

func networks(hostname string) []string {
	if strings.Compare(hostname, "localhost") == 0 || hostname == "" {
		return []string{"tcp"}
	}
	addrs, err := net.LookupIP(hostname)
	if err != nil {
		log.Fatal(err)
	}
	nets := make([]string, 0, 2)
	ipv4 := false
	ipv6 := false
	for _, addr := range addrs {
		// addr == net.IPv4len doesn't work because all addrs use IPv6 format.
		if addr.To4() != nil {
			ipv4 = true
		} else {
			ipv6 = true
		}
	}
	if ipv4 {
		nets = append(nets, "tcp4")
	}
	if ipv6 {
		nets = append(nets, "tcp6")
	}
	return nets
}
