package main

import (
	"crypto/rand"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"time"
)

var decoyTargets = []string{
	// === GOOGLE FONTS (30 шт — CSS2, очень лёгкие) ===
	"https://fonts.googleapis.com/css2?family=Roboto:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Open+Sans:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Inter:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Poppins:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Montserrat:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Lato:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Oswald:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Raleway:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Ubuntu:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=DM+Sans:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Space+Mono:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Alegreya:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Merriweather:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Playfair+Display:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Source+Sans+3:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Barlow:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Figtree:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Manrope:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Work+Sans:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Archivo:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Nunito:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Quicksand:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=PT+Sans:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=IBM+Plex+Sans:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Lexend:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Urbanist:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Outfit:wght@400;700&display=swap",
	"https://fonts.googleapis.com/css2?family=Bebas+Neue:wght@400&display=swap",
	"https://fonts.googleapis.com/css2?family=Anton:wght@400&display=swap",

	// === CDNJS (50 шт — самые популярные библиотеки 2026) ===
	"https://cdnjs.cloudflare.com/ajax/libs/jquery/3.7.1/jquery.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/bootstrap/5.3.3/css/bootstrap.min.css",
	"https://cdnjs.cloudflare.com/ajax/libs/bootstrap/5.3.3/js/bootstrap.bundle.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/font-awesome/6.6.0/css/all.min.css",
	"https://cdnjs.cloudflare.com/ajax/libs/lodash.js/4.17.21/lodash.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/swiper/11.1.14/swiper-bundle.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/animate.css/4.1.1/animate.min.css",
	"https://cdnjs.cloudflare.com/ajax/libs/moment.js/2.30.1/moment.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/chart.js/4.4.1/chart.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/axios/1.7.7/axios.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/vue/3.5.12/vue.global.prod.js",
	"https://cdnjs.cloudflare.com/ajax/libs/react/18.3.1/react.production.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/dayjs/1.11.13/dayjs.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/gsap/3.12.5/gsap.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/three.js/r134/three.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/underscore/1.13.6/underscore-min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/backbone.js/1.6.0/backbone-min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/handlebars.js/4.7.8/handlebars.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/modernizr/2.8.3/modernizr.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/normalize/8.0.1/normalize.min.css",
	"https://cdnjs.cloudflare.com/ajax/libs/tailwindcss/3.4.14/tailwind.min.css",
	"https://cdnjs.cloudflare.com/ajax/libs/alpinejs/3.14.6/cdn.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/htmx/2.0.3/htmx.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/luxon/3.5.0/luxon.min.js",
	"https://cdnjs.cloudflare.com/ajax/libs/immutable/5.0.3/immutable.min.js",

	// === jsDelivr (40 шт — топ npm-пакетов) ===
	"https://cdn.jsdelivr.net/npm/bootstrap@5.3.3/dist/css/bootstrap.min.css",
	"https://cdn.jsdelivr.net/npm/bootstrap@5.3.3/dist/js/bootstrap.bundle.min.js",
	"https://cdn.jsdelivr.net/npm/swiper@11.1.14/swiper-bundle.min.js",
	"https://cdn.jsdelivr.net/npm/@popperjs/core@2.11.8/dist/umd/popper.min.js",
	"https://cdn.jsdelivr.net/npm/lodash@4.17.21/lodash.min.js",
	"https://cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.min.js",
	"https://cdn.jsdelivr.net/npm/axios@1.7.7/dist/axios.min.js",
	"https://cdn.jsdelivr.net/npm/dayjs@1.11.13/dayjs.min.js",
	"https://cdn.jsdelivr.net/npm/gsap@3.12.5/dist/gsap.min.js",
	"https://cdn.jsdelivr.net/npm/alpinejs@3.14.6/dist/cdn.min.js",
	"https://cdn.jsdelivr.net/npm/htmx.org@2.0.3/dist/htmx.min.js",
	"https://cdn.jsdelivr.net/npm/react@18.3.1/umd/react.production.min.js",
	"https://cdn.jsdelivr.net/npm/vue@3.5.12/dist/vue.global.prod.js",
	"https://cdn.jsdelivr.net/npm/moment@2.30.1/moment.min.js",
	"https://cdn.jsdelivr.net/npm/font-awesome@4.7.0/css/font-awesome.min.css",

	// === PICSUM (маленькие стабильные фото, 20 шт) ===
	"https://picsum.photos/id/1/200/300",
	"https://picsum.photos/id/10/200/300",
	"https://picsum.photos/id/20/200/300",
	"https://picsum.photos/id/30/200/300",
	"https://picsum.photos/id/40/200/300",
	"https://picsum.photos/id/50/200/300",
	"https://picsum.photos/id/60/200/300",
	"https://picsum.photos/id/70/200/300",
	"https://picsum.photos/id/80/200/300",
	"https://picsum.photos/id/90/200/300",
	"https://picsum.photos/id/100/200/300",
	"https://picsum.photos/id/101/200/300",
	"https://picsum.photos/id/102/200/300",
	"https://picsum.photos/id/103/200/300",
	"https://picsum.photos/id/104/200/300",
	"https://picsum.photos/id/106/200/300",
	"https://picsum.photos/id/107/200/300",
	"https://picsum.photos/id/108/200/300",
	"https://picsum.photos/id/109/200/300",
	"https://picsum.photos/id/110/200/300",
	// === HEAVY DOWNLOADS (10 шт — 100-200MB файлы) ===
	"https://speed.hetzner.de/100MB.bin",
	"https://download.thinkbroadband.com/100MB.zip",
	"https://download.thinkbroadband.com/200MB.zip",
	"https://testfiledownload.com/wp-content/uploads/2020/06/100MB.zip",
	"https://testfiledownload.com/wp-content/uploads/2020/06/200MB.zip",
	"https://speedtest.newark.linode.com/100MB-newark.bin",
	"https://speedtest.atlanta.linode.com/100MB-atlanta.bin",
	"https://speedtest.london.linode.com/100MB-london.bin",
	"https://speedtest.frankfurt.linode.com/100MB-frankfurt.bin",
	"https://speedtest.singapore.linode.com/100MB-singapore.bin",
}

func runDecoyTraffic(listenAddr string) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		host = "127.0.0.1"
		port = "1080"
	}
	if host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	proxyURL, err := url.Parse("socks5://" + net.JoinHostPort(host, port))
	if err != nil {
		log.Printf("[decoy] Failed to parse proxy URL: %v", err)
		return
	}

	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	log.Printf("Decoy traffic enabled: starting background requests via %s", listenAddr)

	for {
		rD, err := rand.Int(rand.Reader, big.NewInt(2000))
		if err != nil {
			rD = big.NewInt(300)
		}
		delay := time.Duration(rD.Int64()) * time.Millisecond
		time.Sleep(delay)
		rD, err = rand.Int(rand.Reader, big.NewInt(int64(len(decoyTargets))))
		if err != nil {
			rD = big.NewInt(5)
		}
		target := decoyTargets[rD.Int64()]
		log.Printf("[decoy] -> %s", target)
		resp, err := client.Get(target)
		if err == nil {
			go resp.Body.Close()
		} else {
			log.Printf("[decoy] ERR decoy %s: %v", target, err)
		}
	}
}
