package proto

// MeshStat 仅包含实际采集的链路状态；心跳报文不得携带密码。
type MeshStat struct {
	Radio  string     `json:"radio"`
	Ifname string     `json:"ifname"`
	MAC    string     `json:"mac,omitempty"`
	Up     bool       `json:"up"`
	Error  string     `json:"error,omitempty"`
	Peers  []MeshPeer `json:"peers"`
}

type MeshPeer struct {
	MAC         string `json:"mac"`
	Established bool   `json:"established"`
	Signal      int    `json:"signal_dbm,omitempty"`
}
