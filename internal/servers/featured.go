// Package servers holds the featured server catalog shown on the website.
package servers

type Server struct {
	Host   string `json:"host"`
	Port   string `json:"port"`
	Target string `json:"target"`
	Name   string `json:"name"`
	Icon   string `json:"icon,omitempty"`
}

// Featured returns an independent copy of the built-in catalog; no network is needed.
func Featured() []Server {
	return []Server{
		{Host: "zeqa.net", Port: "19132", Target: "zeqa.net", Name: "Zeqa", Icon: "/assets/zeqa_icon.png"},
		{Host: "play.lbsg.net", Port: "19132", Target: "play.lbsg.net", Name: "Lifeboat"},
		{Host: "play.nethergames.org", Port: "19132", Target: "play.nethergames.org", Name: "NetherGames"},
		{Host: "geo.hivebedrock.network", Port: "19132", Target: "geo.hivebedrock.network", Name: "The Hive", Icon: "/assets/thehive_icon.png"},
	}
}
