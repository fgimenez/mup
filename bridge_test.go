package mup

import (
	"time"

	. "gopkg.in/check.v1"
	"labix.org/v2/mgo"
)

type StationSuite struct {
	LineServerSuite
	MgoSuite

	session *mgo.Session
	config  *Config
	station *Station
}

var _ = Suite(&StationSuite{})

func (s *StationSuite) SetUpSuite(c *C) {
	s.LineServerSuite.SetUpSuite(c)
	s.MgoSuite.SetUpSuite(c)
}

func (s *StationSuite) TearDownSuite(c *C) {
	s.LineServerSuite.TearDownSuite(c)
	s.MgoSuite.TearDownSuite(c)
}

func (s *StationSuite) SetUpTest(c *C) {
	s.LineServerSuite.SetUpTest(c)
	s.MgoSuite.SetUpTest(c)

	SetDebug(true)
	SetLogger(c)

	var err error
	s.session, err = mgo.Dial("localhost:50017")
	c.Assert(err, IsNil)

	s.config = &Config{
		Database: s.session.DB("mup"),
		Refresh:  -1, // Manual refreshing for testing.
	}

	err = s.config.Database.C("servers").Insert(M{
		"name":     "testserver",
		"host":     s.Addr.String(),
		"password": "password",
	})
	c.Assert(err, IsNil)

	s.station, err = Start(s.config)
	c.Assert(err, IsNil)

	lserver := s.LineServer(0)
	readUser(c, lserver)
}

func readUser(c *C, lserver *LineServer) {
	c.Assert(lserver.ReadLine(), Equals, "PASS password")
	c.Assert(lserver.ReadLine(), Equals, "NICK mup")
	c.Assert(lserver.ReadLine(), Equals, "USER mup 0 0 :Mup Pet")
}

func sendWelcome(c *C, lserver *LineServer) {
	lserver.SendLine(":n.net 001 mup :Welcome!")
}

func handshake(c *C, lserver *LineServer) {
	readUser(c, lserver)
	sendWelcome(c, lserver)
}

func (s *StationSuite) TearDownTest(c *C) {
	s.LineServerSuite.TearDownTest(c)
	if s.station != nil {
		s.station.Stop()
	}
	s.session.Close()
	s.config.Database.Session.Close()
	s.MgoSuite.TearDownTest(c)
}

func (s *StationSuite) TestConnect(c *C) {
	// SetUpTest does it all.
}

func (s *StationSuite) TestNickInUse(c *C) {
	lserver := s.LineServer(0)
	lserver.SendLine(":n.net 433 * mup :Nickname is already in use.")
	c.Assert(lserver.ReadLine(), Equals, "NICK mup_")
	lserver.SendLine(":n.net 433 * mup_ :Nickname is already in use.")
	c.Assert(lserver.ReadLine(), Equals, "NICK mup__")
}

func (s *StationSuite) TestPingPong(c *C) {
	lserver := s.LineServer(0)
	lserver.SendLine("PING :foo")
	c.Assert(lserver.ReadLine(), Equals, "PONG :foo")
}

func (s *StationSuite) TestPingPongPostAuth(c *C) {
	lserver := s.LineServer(0)
	sendWelcome(c, lserver)
	lserver.SendLine("PING :foo")
	c.Assert(lserver.ReadLine(), Equals, "PONG :foo")
}

func (s *StationSuite) TestQuit(c *C) {
	s.station.Stop()
	c.Assert(s.LineServer(0).ReadLine(), Equals, "<LineServer closed: <nil>>")
}

func (s *StationSuite) TestQuitPostAuth(c *C) {
	lserver := s.LineServer(0)
	sendWelcome(c, lserver)
	lserver.SendLine("PING :roundtrip")
	c.Assert(lserver.ReadLine(), Equals, "PONG :roundtrip")
	stopped := make(chan error)
	go func() {
		stopped <- s.station.Stop()
	}()
	c.Assert(lserver.ReadLine(), Equals, "QUIT :brb")
	lserver.Close()
	c.Assert(<-stopped, IsNil)
}

func (s *StationSuite) TestJoinChannel(c *C) {
	lserver := s.LineServer(0)
	sendWelcome(c, lserver)
	lserver.SendLine(":n.net 001 mup :Welcome!")

	servers := s.Session.DB("").C("servers")
	err := servers.Update(
		M{"name": "testserver"},
		M{"$set": M{"channels": []M{{"name": "#c1"}, {"name": "#c2"}}}},
	)
	c.Assert(err, IsNil)

	s.station.RefreshServers()
	c.Assert(lserver.ReadLine(), Equals, "JOIN #c1,#c2")

	// Confirm it joined both channels.
	lserver.SendLine(":mup!~mup@10.0.0.1 JOIN #c1")
	lserver.SendLine(":mup!~mup@10.0.0.1 JOIN #c2")

	err = servers.Update(
		M{"name": "testserver"},
		M{"$set": M{"channels": []M{{"name": "#c1"}, {"name": "#c3"}}}},
	)
	c.Assert(err, IsNil)

	s.station.RefreshServers()
	c.Assert(lserver.ReadLine(), Equals, "JOIN #c3")
	c.Assert(lserver.ReadLine(), Equals, "PART #c2")

	// Do not confirm, forcing it to retry.
	s.station.RefreshServers()
	c.Assert(lserver.ReadLine(), Equals, "JOIN #c3")
	c.Assert(lserver.ReadLine(), Equals, "PART #c2")
}

func waitFor(condition func() bool) {
	now := time.Now()
	end := now.Add(1 * time.Second)
	for now.Before(end) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
		now = time.Now()
	}
}

func (s *StationSuite) TestIncoming(c *C) {
	lserver := s.LineServer(0)
	sendWelcome(c, lserver)

	lserver.SendLine(":somenick!~someuser@somehost PRIVMSG mup :Hello mup!")

	incoming := s.Session.DB("").C("incoming")

	waitFor(func() bool {
		count, err := incoming.Find(nil).Count()
		c.Assert(err, IsNil)
		return count > 0
	})

	var messages []*Message
	err := incoming.Find(nil).All(&messages)
	c.Assert(err, IsNil)

	c.Assert(messages, HasLen, 1)

	messages[0].Id = ""

	c.Assert(messages[0], DeepEquals, &Message{
		Server:  "testserver",
		Prefix:  "somenick!~someuser@somehost",
		Nick:    "somenick",
		User:    "~someuser",
		Host:    "somehost",
		Cmd:     "PRIVMSG",
		Params:  []string{"mup", ":Hello mup!"},
		Target:  "mup",
		Text:    "Hello mup!",
		Bang:    "!",
		MupNick: "mup",
		ToMup: true,
		MupText: "Hello mup!",
	})
}

func (s *StationSuite) TestOutgoing(c *C) {
	// Stop default bridge to test the behavior of outgoing messages on start up.
	s.LineServer(0).Close()
	s.station.Stop()

	servers := s.Session.DB("").C("servers")
	err := servers.Update(
		M{"name": "testserver"},
		M{"$set": M{"channels": []M{{"name": "#test"}}}},
	)
	c.Assert(err, IsNil)

	outgoing := s.Session.DB("").C("outgoing")
	err = outgoing.Insert(&Message{
		Server: "testserver",
		Target: "someone",
		Text:   "Hello there!",
	})
	c.Assert(err, IsNil)

	bridge, err := Start(s.config)
	c.Assert(err, IsNil)
	defer bridge.Stop()

	lserver := s.LineServer(1)
	defer lserver.Close()
	handshake(c, lserver)

	// The initial JOINs is sent before any messages.
	c.Assert(lserver.ReadLine(), Equals, "JOIN #test")

	// Then the message and the PING asking for confirmation.
	c.Assert(lserver.ReadLine(), Equals, "PRIVMSG someone :Hello there!")
	ping := lserver.ReadLine()
	c.Assert(ping, Matches, "PING :sent:[0-9a-f]+")

	// Confirm that the message was observed.
	lserver.SendLine("PONG" + ping[4:])

	// Send another message with the bridge running.
	err = outgoing.Insert(&Message{
		Server: "testserver",
		Target: "someone",
		Text:   "Hello again!",
	})
	c.Assert(err, IsNil)
	c.Assert(lserver.ReadLine(), Equals, "PRIVMSG someone :Hello again!")
	c.Assert(lserver.ReadLine(), Matches, "PING :sent:[0-9a-f]+")

	// Do not confirm it, and restart the bridge.
	lserver.Close()
	bridge.Stop()
	bridge, err = Start(s.config)
	c.Assert(err, IsNil)
	defer bridge.Stop()

	lserver = s.LineServer(2)
	defer lserver.Close()
	handshake(c, lserver)

	// The unconfirmed message is resent.
	c.Assert(lserver.ReadLine(), Equals, "JOIN #test")
	c.Assert(lserver.ReadLine(), Equals, "PRIVMSG someone :Hello again!")
	c.Assert(lserver.ReadLine(), Matches, "PING :sent:[0-9a-f]+")
}

func (s *StationSuite) TestEchoPlugin(c *C) {
	lserver := s.LineServer(0)
	sendWelcome(c, lserver)

	plugins := s.Session.DB("").C("plugins")
	err := plugins.Insert(M{"name": "echo"})
	c.Assert(err, IsNil)

	s.station.RefreshPlugins()

	lserver.SendLine(":somenick!~someuser@somehost PRIVMSG mup :echo Repeat this.")
	c.Assert(lserver.ReadLine(), Equals, "PRIVMSG somenick :Repeat this.")
}
