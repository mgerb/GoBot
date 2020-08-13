package bothandlers

import (
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mgerb/go-discord-bot/server/db"
	"github.com/mgerb/go-discord-bot/server/webserver/model"

	"github.com/bwmarrin/discordgo"
	"github.com/mgerb/go-discord-bot/server/config"
	"github.com/mgerb/go-discord-bot/server/util"
	log "github.com/sirupsen/logrus"
)

const (
	channels                 int = 2     // 1 for mono, 2 for stereo
	sampleRate               int = 48000 // audio sampling rate - apparently a standard for opus
	opusFrameSize            int = 960   // at 48kHz the permitted values are 120, 240, 480, or 960
	maxSoundQueue            int = 10    // max amount of sounds that can be queued at one time
	voiceClipQueuePacketSize int = 2000  // this packet size equates to roughly 40 seconds of audio
)

// ActiveConnections - current active bot connections
// store our connection objects in a map tied to a guild id
var ActiveConnections = make(map[string]*AudioConnection)

// AudioConnection -
type AudioConnection struct {
	Guild             *discordgo.Guild           `json:"guild"`
	Session           *discordgo.Session         `json:"-"`
	VoiceConnection   *discordgo.VoiceConnection `json:"-"`
	CurrentChannel    *discordgo.Channel         `json:"current_channel"`
	Sounds            map[string]*AudioClip      `json:"-"`
	SoundQueue        chan string                `json:"-"`
	VoiceClipQueue    chan *discordgo.Packet     `json:"-"`
	SoundPlayingLock  bool                       `json:"-"`
	AudioListenerLock bool                       `json:"-"`
	Mutex             *sync.Mutex                `json:"-"` // mutex for single audio connection
}

// AudioClip -
type AudioClip struct {
	Name      string
	Extension string
	Content   [][]byte
}

// SoundsHandler -
func SoundsHandler(s *discordgo.Session, m *discordgo.MessageCreate) {

	// get channel state to get guild id
	c, err := s.State.Channel(m.ChannelID)
	if err != nil {
		// Could not find channel.
		log.Error("Unable to find channel.")
		return
	}

	// check to see if active connection object exists
	if _, ok := ActiveConnections[c.GuildID]; !ok {

		// Find the guild for that channel.
		newGuild, err := s.State.Guild(c.GuildID)

		if err != nil {
			log.Error(err)
			return
		}

		// create new connection instance
		ActiveConnections[c.GuildID] = &AudioConnection{
			Guild:             newGuild,
			Session:           s,
			Sounds:            make(map[string]*AudioClip, 0),
			SoundQueue:        make(chan string, maxSoundQueue),
			Mutex:             &sync.Mutex{},
			AudioListenerLock: false,
		}
	}

	ActiveConnections[c.GuildID].handleMessage(m)
}

func (conn *AudioConnection) handleMessage(m *discordgo.MessageCreate) {

	// check if valid command
	if strings.HasPrefix(m.Content, config.Config.BotPrefix) {

		command := strings.TrimPrefix(m.Content, config.Config.BotPrefix)

		switch command {

		case "summon":
			conn.summon(m)

		case "dismiss":
			conn.dismiss()

		case "clip":
			conn.clipAudio(m)

		case "random":
			conn.PlayRandomAudio(m, nil)

		// restart command handled in bot file - needed here to break out
		case "restart":
			break

		default:
			conn.PlayAudio(command, m, nil)
		}
	}
}

// dismiss bot from currnet channel if it's in one
func (conn *AudioConnection) dismiss() {
	if conn.VoiceConnection != nil && !conn.SoundPlayingLock && len(conn.SoundQueue) == 0 {
		conn.VoiceConnection.Disconnect()
	}
}

// summon bot to channel that user is currently in
func (conn *AudioConnection) summon(m *discordgo.MessageCreate) {

	// Join the channel the user issued the command from if not in it
	if conn.VoiceConnection == nil || conn.VoiceConnection.ChannelID != m.ChannelID {

		var err error

		// Find the channel that the message came from.
		c, err := conn.Session.State.Channel(m.ChannelID)
		if err != nil {
			// Could not find channel.
			log.Error("User channel not found.")
			return
		}

		// Find the guild for that channel.
		g, err := conn.Session.State.Guild(c.GuildID)
		if err != nil {
			log.Error(err)
			return
		}

		// Look for the message sender in that guilds current voice states
		for _, vs := range g.VoiceStates {
			if vs.UserID == m.Author.ID {

				conn.VoiceConnection, err = conn.Session.ChannelVoiceJoin(g.ID, vs.ChannelID, false, false)

				if err != nil {
					log.Error(err)
					return
				}

				// set the current channel
				conn.CurrentChannel = c

				// start listening to audio if not locked
				if !conn.AudioListenerLock {
					go conn.startAudioListener()
				}
			}
		}

	}
}

// play a random sound clip
func (conn *AudioConnection) PlayRandomAudio(m *discordgo.MessageCreate, userID *string) {
	files, _ := ioutil.ReadDir(config.Config.SoundsPath)
	if len(files) > 0 {
		randomIndex := rand.Intn(len(files))
		arr := strings.Split(files[randomIndex].Name(), ".")
		if len(arr) > 0 && arr[0] != "" {
			conn.PlayAudio(arr[0], m, userID)
		}
	}
}

// PlayAudio - play audio in channel that user is in
// if MessageCreate is null play in current channel
func (conn *AudioConnection) PlayAudio(soundName string, m *discordgo.MessageCreate, userID *string) {

	// summon bot to channel if new message passed in
	if m != nil {
		conn.summon(m)
	} else if conn.VoiceConnection == nil || !conn.VoiceConnection.Ready {
		return
	}

	// check if sound exists in memory
	if _, ok := conn.Sounds[soundName]; !ok {
		// try to load the sound if not found in memory
		err := conn.loadFile(soundName)

		if err != nil {
			log.Error(err)
			return
		}
	}

	// add sound to queue if queue isn't full
	select {
	case conn.SoundQueue <- soundName:

		var newUserID string
		fromWebUI := false

		// from discord
		if m != nil && m.Author != nil {
			newUserID = m.Author.ID
		} else {
			fromWebUI = true
			newUserID = *userID
		}

		// log event when user plays sound clip
		err := model.LogSoundPlayedEvent(db.GetConn(), newUserID, soundName, fromWebUI)

		if err != nil {
			log.Error(err)
		}

	default:
		break
	}

	// start playing sounds in queue if not already playing
	if !conn.SoundPlayingLock {
		conn.playSoundsInQueue()
	}

}

// playSoundsInQueue - play sounds until audio queue is empty
func (conn *AudioConnection) playSoundsInQueue() {
	conn.toggleSoundPlayingLock(true)

	// Start speaking.
	conn.VoiceConnection.Speaking(true)

	for {
		select {
		case newSoundName := <-conn.SoundQueue:

			if !conn.VoiceConnection.Ready {
				return
			}

			// Send the buffer data.
			for _, buff := range conn.Sounds[newSoundName].Content {
				conn.VoiceConnection.OpusSend <- buff
			}

			// Sleep for a specificed amount of time before ending.
			time.Sleep(100 * time.Millisecond)

		default:
			// Stop speaking
			conn.VoiceConnection.Speaking(false)
			conn.toggleSoundPlayingLock(false)
			return
		}
	}
}

func (conn *AudioConnection) toggleSoundPlayingLock(playing bool) {
	conn.Mutex.Lock()
	conn.SoundPlayingLock = playing
	conn.Mutex.Unlock()
}

// load audio file into memory
func (conn *AudioConnection) loadFile(fileName string) error {

	extension, err := util.GetFileExtension(config.Config.SoundsPath, fileName)

	if err != nil {
		return err
	}

	opusData, err := util.GetFileOpusData(path.Join(config.Config.SoundsPath, fileName+extension), channels, opusFrameSize, sampleRate)

	if err != nil {
		return err
	}

	conn.Sounds[fileName] = &AudioClip{
		Content:   opusData,
		Name:      fileName,
		Extension: extension,
	}

	return nil
}

func (conn *AudioConnection) clipAudio(m *discordgo.MessageCreate) {
	if len(conn.VoiceClipQueue) < 10 {
		conn.Session.ChannelMessageSend(m.ChannelID, "Clip failed.")
	} else {
		writePacketsToFile(m.Author.Username, conn.VoiceClipQueue)
		conn.Session.ChannelMessageSend(m.ChannelID, "Sound clipped!")
	}
}

func writePacketsToFile(username string, packets chan *discordgo.Packet) {

	// create clips folder if it does not exist
	if _, err := os.Stat(config.Config.ClipsPath); os.IsNotExist(err) {
		os.Mkdir(config.Config.ClipsPath, os.ModePerm)
	}

	opusData := map[uint32][][]byte{}

loop:
	for {
		select {
		case p := <-packets:
			// separate opus data for each channel
			opusData[p.SSRC] = append(opusData[p.SSRC], p.Opus)
		default:
			break loop
		}
	}

	for key, opus := range opusData {

		pcmData, err := util.OpusToPCM(opus, sampleRate, channels)

		if err != nil {
			log.Error(err)
			continue
		}

		// construct filename
		timestamp := time.Now().UTC().Format("2006-01-02") + "-" + strconv.Itoa(int(time.Now().Unix()))
		filename := config.Config.ClipsPath + "/" + timestamp + "-" + strconv.Itoa(int(key)) + "-" + username + ".wav"

		err = util.SavePCMToWavFile(pcmData, filename, sampleRate, channels)

		if err != nil {
			log.Error(err)
		}
	}
}

// start listening to the voice channel
func (conn *AudioConnection) startAudioListener() {

	conn.AudioListenerLock = true

	if conn.VoiceClipQueue == nil {
		conn.VoiceClipQueue = make(chan *discordgo.Packet, voiceClipQueuePacketSize)
	}

	// create new channel to watch for voice connection
	// when voice connection is not ready the loop will exit
	exitChan := make(chan bool)

	go func() {
		for {
			if !conn.VoiceConnection.Ready {
				exitChan <- true
				break
			}
			time.Sleep(time.Second * 1)
		}
	}()

loop:
	for {

		select {
		// grab incoming audio
		case opusChannel, ok := <-conn.VoiceConnection.OpusRecv:
			if !ok {
				continue
			}

			// if channel is full trim off from beginning
			if len(conn.VoiceClipQueue) == cap(conn.VoiceClipQueue) {
				<-conn.VoiceClipQueue
			}

			// add current packet to channel queue
			conn.VoiceClipQueue <- opusChannel

		// check if voice connection fails then break out of audio listener
		case <-exitChan:
			break loop
		}
	}

	// remove lock upon exit
	conn.AudioListenerLock = false
}
