package app

import (
	"archive/zip"
	"github.com/restartfu/decryptmypack/app/minecraft"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var downloading = map[string]chan struct{}{}

func (a *App) download(w http.ResponseWriter, r *http.Request) {
	target := r.FormValue("target")
	if len(target) == 0 {
		http.Error(w, "missing target", http.StatusBadRequest)
		return
	}

	if len(strings.Split(target, ":")) == 2 {
		target = strings.Split(target, ":")[0]
	}

	addrs, _ := net.LookupHost(target)
	addr := addrs[0]

	if c, ok := downloading[addr]; ok {
		<-c
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")

	if f, err := os.Stat("packs/" + addr + "/" + addr + ".zip"); err == nil && time.Now().Sub(f.ModTime()) <= time.Minute*10 {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+addr+".zip\"")
		http.ServeFile(w, r, "packs/"+addr+"/"+addr+".zip")
		return
	}

	downloading[addr] = make(chan struct{})
	defer delete(downloading, addr)

	conn, err := minecraft.Connect(target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	packs := conn.ResourcePacks()
	if len(packs) == 0 {
		http.Error(w, "The server does not have any resource pack.", http.StatusNotFound)
		return
	}

	_ = os.Mkdir("packs/"+addr, 0777)
	f, err := os.OpenFile("packs/"+addr+"/"+addr+".zip", os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	zipFile := zip.NewWriter(f)

	for _, pack := range packs {
		buf, err := minecraft.EncodePack(pack)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if pack.Encrypted() {
			newBuf, err := minecraft.DecryptPack(buf, pack.ContentKey())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if len(newBuf) > 0 {
				buf = newBuf
			}
		}

		p, err := zipFile.Create(pack.Name() + ".zip")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, err = p.Write(buf)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+addr+".zip\"")

	_ = zipFile.Close()
	_, _ = f.Seek(0, 0)
	_, _ = io.Copy(w, f)
	_ = f.Close()
}
