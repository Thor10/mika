package tracker

import (
	"bytes"
	"fmt"
	"git.totdev.in/totv/mika/conf"
	"git.totdev.in/totv/mika/db"
	"git.totdev.in/totv/mika/util"
	"github.com/garyburd/redigo/redis"
	"log"
	"net"
	"strings"
	"sync"
)

type Peer struct {
	db.Queued
	sync.RWMutex
	SpeedUP        float64 `redis:"speed_up" json:"speed_up"`
	SpeedDN        float64 `redis:"speed_dn" json:"speed_dn"`
	SpeedUPMax     float64 `redis:"speed_up" json:"speed_up_max"`
	SpeedDNMax     float64 `redis:"speed_dn" json:"speed_dn_max"`
	Uploaded       uint64  `redis:"uploaded" json:"uploaded"`
	Downloaded     uint64  `redis:"downloaded" json:"downloaded"`
	UploadedLast   uint64  `redis:"-" json:"-"`
	DownloadedLast uint64  `redis:"-" json:"-"`
	Corrupt        uint64  `redis:"corrupt" json:"corrupt"`
	IP             string  `redis:"ip" json:"ip"`
	Port           uint64  `redis:"port" json:"port"`
	Left           uint64  `redis:"left" json:"left"`
	Announces      uint64  `redis:"announces" json:"announces"`
	TotalTime      uint32  `redis:"total_time" json:"total_time"`
	AnnounceLast   int32   `redis:"last_announce" json:"last_announce"`
	AnnounceFirst  int32   `redis:"first_announce" json:"first_announce"`
	New            bool    `redis:"new" json:"-"`
	PeerID         string  `redis:"peer_id" json:"peer_id"`
	Active         bool    `redis:"active"  json:"active"`
	Username       string  `redis:"username"  json:"username"`
	UserID         uint64  `redis:"user_id"  json:"user_id"`
	TorrentID      uint64  `redis:"torrent_id" json:"torrent_id"`
	KeyPeer        string  `redis:"-" json:"-"`
	KeyTimer       string  `redis:"-" json:"-"`
}

// Update the stored values with the data from an announce
func (peer *Peer) Update(announce *AnnounceRequest) (uint64, uint64) {
	peer.Lock()
	defer peer.Unlock()
	cur_time := util.Unixtime()
	peer.PeerID = announce.PeerID
	peer.Announces++

	ul_diff := uint64(0)
	dl_diff := uint64(0)

	if announce.Event == STARTED {
		peer.Uploaded = announce.Uploaded
		peer.Downloaded = announce.Downloaded
	} else if announce.Uploaded < peer.Uploaded || announce.Downloaded < peer.Downloaded {
		peer.Uploaded = announce.Uploaded
		peer.Downloaded = announce.Downloaded
	} else {
		if announce.Uploaded != peer.Uploaded {
			ul_diff = announce.Uploaded - peer.Uploaded
			peer.Uploaded = announce.Uploaded
		}
		if announce.Downloaded != peer.Downloaded {
			dl_diff = announce.Downloaded - peer.Downloaded
			peer.Downloaded = announce.Downloaded
		}

	}
	peer.IP = announce.IPv4.String()
	peer.Port = announce.Port
	peer.Corrupt = announce.Corrupt
	peer.Left = announce.Left
	peer.SpeedUP = util.EstSpeed(peer.AnnounceLast, cur_time, ul_diff)
	peer.SpeedDN = util.EstSpeed(peer.AnnounceLast, cur_time, dl_diff)
	if peer.SpeedUP > peer.SpeedUPMax {
		peer.SpeedUPMax = peer.SpeedUP
	}
	if peer.SpeedDN > peer.SpeedDNMax {
		peer.SpeedDNMax = peer.SpeedDN
	}

	// Must be active to have a real time delta
	if peer.Active && peer.AnnounceLast > 0 {
		time_diff := uint64(util.Unixtime() - peer.AnnounceLast)
		// Ignore long periods of inactivity
		if time_diff < (uint64(conf.Config.AnnInterval) * 4) {
			peer.TotalTime += uint32(time_diff)
		}
	}
	if announce.Event == STOPPED {
		peer.Active = false
	}
	return ul_diff, dl_diff
}

func (peer *Peer) SetUserID(user_id uint64, username string) {
	peer.Lock()
	defer peer.Unlock()
	peer.UserID = user_id
	peer.Username = username
}

func (peer *Peer) Sync(r redis.Conn) {
	r.Send(
		"HMSET", peer.KeyPeer,
		"ip", peer.IP,
		"port", peer.Port,
		"left", peer.Left,
		"first_announce", peer.AnnounceFirst,
		"last_announce", peer.AnnounceLast,
		"total_time", peer.TotalTime,
		"speed_up", peer.SpeedUP,
		"speed_dn", peer.SpeedDN,
		"speed_up_max", peer.SpeedUPMax,
		"speed_dn_max", peer.SpeedDNMax,
		"active", peer.Active,
		"uploaded", peer.Uploaded,
		"downloaded", peer.Downloaded,
		"corrupt", peer.Corrupt,
		"username", peer.Username,
		"user_id", peer.UserID, // Shouldn't need to be here
		"peer_id", peer.PeerID, // Shouldn't need to be here
		"torrent_id", peer.TorrentID, // Shouldn't need to be here
	)
}

func (peer *Peer) IsHNR() bool {
	return peer.Downloaded > conf.Config.HNRMinBytes && peer.Left > 0 && peer.TotalTime < uint32(conf.Config.HNRThreshold)
}

func (peer *Peer) IsSeeder() bool {
	return peer.Left == 0
}

func (peer *Peer) AddHNR(r redis.Conn, torrent_id uint64) {
	r.Send("SADD", fmt.Sprintf("t:u:hnr:%d", peer.UserID), torrent_id)
	util.Debug("Added HnR:", torrent_id, peer.UserID)
}

// Generate a compact peer field array containing the byte representations
// of a peers IP+Port appended to each other
func MakeCompactPeers(peers []*Peer, skip_id string) []byte {
	var out_buf bytes.Buffer
	for _, peer := range peers {
		if peer.Port <= 0 {
			// FIXME Why does empty peer exist with 0 port??
			continue
		}
		if peer.PeerID == skip_id {
			continue
		}

		out_buf.Write(net.ParseIP(peer.IP).To4())
		out_buf.Write([]byte{byte(peer.Port >> 8), byte(peer.Port & 0xff)})
	}
	return out_buf.Bytes()
}

// Generate a new instance of a peer from the redis reply if data is contained
// within, otherwise just return a default value peer
func MakePeer(redis_reply interface{}, torrent_id uint64, info_hash string, peer_id string) (*Peer, error) {
	peer := &Peer{
		PeerID:        peer_id,
		Active:        false,
		Announces:     0,
		SpeedUP:       0,
		SpeedDN:       0,
		SpeedUPMax:    0,
		SpeedDNMax:    0,
		Uploaded:      0,
		Downloaded:    0,
		Left:          0,
		Corrupt:       0,
		Username:      "",
		IP:            "127.0.0.1",
		Port:          0,
		AnnounceFirst: util.Unixtime(),
		AnnounceLast:  util.Unixtime(),
		TotalTime:     0,
		UserID:        0,
		TorrentID:     torrent_id,
		KeyPeer:       fmt.Sprintf("t:p:%s:%s", info_hash, peer_id),
		KeyTimer:      fmt.Sprintf("t:ptimeout:%s:%s", info_hash, peer_id),
	}

	values, err := redis.Values(redis_reply, nil)
	if err != nil {
		log.Println("makePeer: Failed to parse peer reply: ", err)
		return peer, err_parse_reply
	}
	if values != nil {
		err := redis.ScanStruct(values, peer)
		if err != nil {
			log.Println("makePeer: Failed to scan peer struct: ", err)
			return peer, err_cast_reply
		} else {
			peer.PeerID = peer_id
		}
	}
	return peer, nil
}

// Checked if the clients peer_id prefix matches the client prefixes
// stored in the white lists
func (t *Tracker) IsValidClient(r redis.Conn, peer_id string) bool {

	for _, client_prefix := range t.Whitelist {
		if strings.HasPrefix(peer_id, client_prefix) {
			return true
		}
	}

	log.Println("IsValidClient: Got non-whitelisted client:", peer_id)
	return false
}