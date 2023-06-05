package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"text/template"
	"time"

	"slices"

	"github.com/bwmarrin/discordgo"
	slexp "golang.org/x/exp/slices"
)

var (
	token = flag.String("token", "", "Token to use when connecting")
)

type POTD struct {
	Date time.Time `json:"date"`
	Name string    `json:"name"`
	URL  string    `json:"url"`
	POTW bool      `json:"potw"`
}

type Config struct {
	WebhookID     string `json:"webhook_url"`
	WebhookSecret string `json:"secret"`

	FuturePOTDs []*POTD `json:"future_potds"`
}

const (
	webhookMsg = `{{if .POTW}}<@&1106304624958394368>{{else}}<@&1106147993071140865>{{end}} - {{if gt (len .POTDs) 1}}Problemele{{else}}Problema{{end}} {{if .POTW}}săptămânii{{else}}zilei{{end}} ({{.Date.Format "2006-01-02"}}):
{{range .POTDs}}* {{.Name}} - <{{.URL}}>
{{end}}

Mult succes!`
)

var (
	webhookTempl = template.Must(template.New("potd").Parse(webhookMsg))
)

var configMu sync.RWMutex
var config Config
var s *discordgo.Session

func SaveConfig() error {
	f, err := os.OpenFile("config.json", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "\t")
	if err := enc.Encode(config); err != nil {
		return err
	}
	return f.Close()
}

func LoadConfig() error {
	f, err := os.Open("config.json")
	if err != nil {
		return err
	}
	if err := json.NewDecoder(f).Decode(&config); err != nil {
		return err
	}
	return nil
}

func getDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 7, 0, 0, 0, t.Location()) // at 7am local time
}

var (
	defaultMemberPerm int64 = discordgo.PermissionManageServer
	commands                = []*discordgo.ApplicationCommand{
		{
			Name:        "add-problem",
			Description: "Add problem to future problems list",

			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "name",
					Description: "Name of the problem",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "url",
					Description: "URL of the problem",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "date",
					Description: "Date when it should be sent (format: `yyyy-mm-dd`)",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionBoolean,
					Name:        "potw",
					Description: "It's actually a problem of the week, not of the day",
				},
			},

			DefaultMemberPermissions: &defaultMemberPerm,
		},
		{
			Name:        "list-problems",
			Description: "List all future problems of the day/week",

			DefaultMemberPermissions: &defaultMemberPerm,
		},
		{
			Name:        "delete-problem",
			Description: "Delete problem of specified name in the `list-problems` list",

			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "name",
					Description: "Name of the problem",
					Required:    true,
				},
			},

			DefaultMemberPermissions: &defaultMemberPerm,
		},
		{
			Name:        "update-problem",
			Description: "Update problem of the specified name  before the one in the `list-problems` list",

			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "name",
					Description: "Name of the problem to update",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "new-name",
					Description: "New name of the problem",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "url",
					Description: "New URL for the problem",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "date",
					Description: "New date for the problem (format: `yyyy-mm-dd`)",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionBoolean,
					Name:        "potw",
					Description: "Mark if it's problem of the week",
					Required:    false,
				},
			},

			DefaultMemberPermissions: &defaultMemberPerm,
		},
		{
			Name:        "push-potd",
			Description: "Force publish POTD date to webhook",

			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "date",
					Description: "Date to publish (format: `yyyy-mm-dd`)",
					Required:    true,
				},
			},

			DefaultMemberPermissions: &defaultMemberPerm,
		},
		{
			Name:        "set-webhook",
			Description: "Set webhook to push to",

			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "url",
					Description: "URL of the webhook",
					Required:    true,
				},
			},

			DefaultMemberPermissions: &defaultMemberPerm,
		},
	}

	commandHandlers = map[string]func(s *discordgo.Session, i *discordgo.InteractionCreate){
		"add-problem": func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			configMu.Lock()
			defer configMu.Unlock()
			potd := &POTD{}
			for _, opt := range i.ApplicationCommandData().Options {
				if opt.Name == "name" {
					potd.Name = opt.StringValue()
					for _, potd := range config.FuturePOTDs {
						if strings.EqualFold(potd.Name, opt.StringValue()) {
							s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
								Type: discordgo.InteractionResponseChannelMessageWithSource,
								Data: &discordgo.InteractionResponseData{
									Content: "ERROR: case-insensitive name must not already appear in current problem of the day list",
								},
							})
							return
						}
					}
				} else if opt.Name == "url" {
					potd.URL = opt.StringValue()
				} else if opt.Name == "date" {
					date, err := time.ParseInLocation(time.DateOnly, opt.StringValue(), time.Local)
					if err != nil {
						s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
							Type: discordgo.InteractionResponseChannelMessageWithSource,
							Data: &discordgo.InteractionResponseData{
								Content: fmt.Sprintf("ERROR: invalid time parameter (probably wrong format): %v", err),
							},
						})
						return
					}
					potd.Date = getDate(date) // at 7am local time
				} else if opt.Name == "potw" {
					potd.POTW = opt.BoolValue()
				} else {
					log.Println("wtf")
				}
			}
			config.FuturePOTDs = append(config.FuturePOTDs, potd)
			slexp.SortStableFunc(config.FuturePOTDs, func(potd1, potd2 *POTD) bool {
				return potd1.Date.Before(potd2.Date)
			})
			if err := SaveConfig(); err != nil {
				log.Printf("Couldn't save config: %v", err)
			}
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Added problem",
				},
			})
		},
		"list-problems": func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			configMu.RLock()
			defer configMu.RUnlock()
			ret := "```\nFuture problems of the day:\nFormat: 'date - name - potd/potw - url'\n\n"

			if len(config.FuturePOTDs) == 0 {
				ret += "No future problems found"
			}

			for _, p := range config.FuturePOTDs {
				potd := "POTD"
				if p.POTW {
					potd = "POTW"
				}
				ret += fmt.Sprintf("%s - %s - %s - %s\n", p.Date.Format(time.DateOnly), p.Name, potd, p.URL)
			}

			ret += "\n```"

			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: ret,
				},
			})
		},
		"delete-problem": func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			configMu.Lock()
			defer configMu.Unlock()
			nameToSearch := ""
			for _, opt := range i.ApplicationCommandData().Options {
				if opt.Name == "name" {
					nameToSearch = opt.StringValue()
				} else if opt.Name == "date" {
					date, err := time.ParseInLocation(time.DateOnly, opt.StringValue(), time.Local)
					if err != nil {
						s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
							Type: discordgo.InteractionResponseChannelMessageWithSource,
							Data: &discordgo.InteractionResponseData{
								Content: fmt.Sprintf("ERROR: invalid time parameter (probably wrong format): %v", err),
							},
						})
						return
					}
					tDate := getDate(date) // at 7am local time
					if time.Now().After(tDate) {
						s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
							Type: discordgo.InteractionResponseChannelMessageWithSource,
							Data: &discordgo.InteractionResponseData{
								Content: "ERROR: date must be in the future",
							},
						})
						return
					}
				}
			}
			if nameToSearch == "" {
				s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "ERROR: no name specified",
					},
				})
				return
			}
			oldLen := len(config.FuturePOTDs)
			config.FuturePOTDs = slices.DeleteFunc(config.FuturePOTDs, func(val *POTD) bool {
				return strings.EqualFold(val.Name, nameToSearch)
			})
			newLen := len(config.FuturePOTDs)
			if newLen == oldLen {
				s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "No problems were deleted",
					},
				})
				return
			}
			if err := SaveConfig(); err != nil {
				log.Printf("Couldn't save config: %v", err)
			}
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Deleted problem",
				},
			})
		},
		"update-problem": func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			configMu.Lock()
			defer configMu.Unlock()
			nameToSearch := ""
			for _, opt := range i.ApplicationCommandData().Options {
				if opt.Name == "name" {
					nameToSearch = opt.StringValue()
				}
			}
			if nameToSearch == "" {
				s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "ERROR: no name specified",
					},
				})
				return
			}
			idx := slices.IndexFunc(config.FuturePOTDs, func(p *POTD) bool {
				return strings.EqualFold(p.Name, nameToSearch)
			})
			if idx == -1 {
				s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "ERROR: Couldn't find problem by name",
					},
				})
				return
			}

			for _, opt := range i.ApplicationCommandData().Options {
				if opt.Name == "new-name" {
					config.FuturePOTDs[idx].Name = opt.StringValue()
				} else if opt.Name == "url" {
					config.FuturePOTDs[idx].URL = opt.StringValue()
				} else if opt.Name == "date" {
					date, err := time.ParseInLocation(time.DateOnly, opt.StringValue(), time.Local)
					if err != nil {
						s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
							Type: discordgo.InteractionResponseChannelMessageWithSource,
							Data: &discordgo.InteractionResponseData{
								Content: fmt.Sprintf("ERROR: invalid time parameter (probably wrong format): %v", err),
							},
						})
						return
					}
					config.FuturePOTDs[idx].Date = getDate(date) // at 7am local time
				} else if opt.Name == "potw" {
					config.FuturePOTDs[idx].POTW = opt.BoolValue()
				}
			}

			if err := SaveConfig(); err != nil {
				log.Printf("Couldn't save config: %v", err)
			}

			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Updated problem",
				},
			})
		},
		"push-potd": func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			var tDate time.Time
			for _, opt := range i.ApplicationCommandData().Options {
				if opt.Name == "date" {
					date, err := time.ParseInLocation(time.DateOnly, opt.StringValue(), time.Local)
					if err != nil {
						s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
							Type: discordgo.InteractionResponseChannelMessageWithSource,
							Data: &discordgo.InteractionResponseData{
								Content: fmt.Sprintf("ERROR: invalid time parameter (probably wrong format): %v", err),
							},
						})
						return
					}
					tDate = getDate(date)
				}
			}
			consumePOTDs(tDate)
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Pushed POTDs",
				},
			})
		},
		"set-webhook": func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			configMu.Lock()
			defer configMu.Unlock()
			for _, opt := range i.ApplicationCommandData().Options {
				if opt.Name == "url" {
					url, err := url.Parse(opt.StringValue())
					if err != nil {
						s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
							Type: discordgo.InteractionResponseChannelMessageWithSource,
							Data: &discordgo.InteractionResponseData{
								Content: fmt.Sprintf("ERROR: invalid url: %v", err),
							},
						})
						return //https://discord.com/api/webhooks/1107288435338784780/oj130B9_wH8AgiCQ5vXmi-f2_WNSKz32O2L0zbmEJiEv4iGkH3u1oUP6_8LfnIdiSTE2
					}
					vals := strings.Split(url.Path, "/")
					if len(vals) < 2 {
						s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
							Type: discordgo.InteractionResponseChannelMessageWithSource,
							Data: &discordgo.InteractionResponseData{
								Content: "ERROR: url is not webhook url",
							},
						})
						return
					}
					config.WebhookID = vals[len(vals)-2]
					config.WebhookSecret = vals[len(vals)-1]
					if err := SaveConfig(); err != nil {
						log.Printf("Couldn't save config: %v", err)
					}
				}
			}
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Updated webhook",
				},
			})
		},
	}
)

func eqDates(t1 time.Time, t2 time.Time) bool {
	return t1.Year() == t2.Year() && t1.Month() == t2.Month() && t1.Day() == t2.Day()
}

func consumePOTDs(day time.Time) {
	configMu.Lock()
	defer configMu.Unlock()
	toDel := []string{}
	potds := []*POTD{}
	potws := []*POTW{}
	for _, potd := range config.FuturePOTDs {
		if eqDates(potd.Date, day) {
			if potd.POTW {
				potws = append(potws, potd)
			} else {
				potds = append(potds, potd)
			}
		}
	}
	if len(potds) > 0 {
		var buf bytes.Buffer
		if err := webhookTempl.Execute(&buf, struct {
			POTW  bool
			Date  time.Time
			POTDs []*POTD
		}{POTW: false, Date: potds[0].Date, POTDs: potds}); err != nil {
			log.Println("Template execute error:", err)
			return
		}
		if _, err := s.WebhookExecute(config.WebhookID, config.WebhookSecret, false, &discordgo.WebhookParams{
			Content: buf.String(),
		}); err != nil {
			log.Println("Webhook error:", err)
			continue
		}
		for _, potd := range potds {
			log.Printf("Sent POTD " + potd.Name)
		}
		toDel = append(toDel, potds...)
	}
	if len(potws) > 0 {
		var buf bytes.Buffer
		if err := webhookTempl.Execute(&buf, struct {
			POTW  bool
			Date  time.Time
			POTDs []*POTD
		}{POTW: true, Date: potws[0].Date, POTDs: potws}); err != nil {
			log.Println("Template execute error:", err)
			return
		}
		if _, err := s.WebhookExecute(config.WebhookID, config.WebhookSecret, false, &discordgo.WebhookParams{
			Content: buf.String(),
		}); err != nil {
			log.Println("Webhook error:", err)
			continue
		}
		for _, potd := range potds {
			log.Printf("Sent POTW " + potd.Name)
		}
		toDel = append(toDel, potws...)
	}
	if len(toDel) == 0 {
		return
	}

	config.FuturePOTDs = slices.DeleteFunc(config.FuturePOTDs, func(p *POTD) bool {
		for _, td := range toDel {
			if strings.EqualFold(td, p.Name) {
				return true
			}
		}
		return false
	})

	if err := SaveConfig(); err != nil {
		log.Printf("Couldn't save config: %v", err)
	}
}

func potdPoller(ctx context.Context) {
	t := time.NewTicker(1 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			log.Printf("Stopping poller")
			t.Stop()
		case <-t.C:
			tTime := getDate(time.Now())
			if time.Now().After(tTime) {
				//log.Printf("Consuming POTDs")
				consumePOTDs(tTime)
			}
		}
	}
}

func main() {
	flag.Parse()
	if token == nil || *token == "" {
		log.Fatalln("No token supplied")
	}
	err := LoadConfig()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Fatalf("Couldn't open config: %v", err)
		}
		if err := SaveConfig(); err != nil {
			log.Fatalf("Couldn't create config file: %v", err)
		}
	}
	defer func() {
		if err := SaveConfig(); err != nil {
			log.Fatalf("Couldn't save config: %v", err)
		}
	}()
	s, err = discordgo.New("Bot " + *token)
	if err != nil {
		log.Fatalf("Couldn't create bot instance: %v", err)
	}

	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)

	s.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Printf("Logged in as: %v#%v", s.State.User.Username, s.State.User.Discriminator)

		go potdPoller(ctx)
	})

	s.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		log.Printf(i.ApplicationCommandData().Name)
		if h, ok := commandHandlers[i.ApplicationCommandData().Name]; ok {
			h(s, i)
		}
	})

	if err := s.Open(); err != nil {
		log.Fatalf("Couldn't open session: %v", err)
	}
	defer s.Close()

	log.Println("Adding commands")
	for _, v := range commands {
		_, err := s.ApplicationCommandCreate(s.State.User.ID, "", v)
		if err != nil {
			log.Panicf("Cannot create '%v' command: %v", v.Name, err)
		}
	}

	<-ctx.Done()

	log.Println("Getting commands to remove")
	registeredCommands, err := s.ApplicationCommands(s.State.User.ID, "")
	if err != nil {
		log.Fatalf("Could not fetch registered commands: %v", err)
	}

	for _, v := range registeredCommands {
		log.Printf("Removing %s", v.Name)
		err := s.ApplicationCommandDelete(s.State.User.ID, "", v.ID)
		if err != nil {
			log.Panicf("Cannot delete '%v' command: %v", v.Name, err)
		}
	}

	log.Printf("Shutting down")
}
