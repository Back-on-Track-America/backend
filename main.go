package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	ics "github.com/arran4/golang-ical"
	"github.com/labstack/echo/v5"
	expo "github.com/oliveroneill/exponent-server-sdk-golang/sdk"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/models"
	"github.com/pocketbase/pocketbase/tools/cron"
)

func announce(app *pocketbase.PocketBase, record *models.Record, token string) {
	client := expo.NewPushClient(nil)

	pushToken, err := expo.NewExponentPushToken(token)

	if err != nil {
		log.Default().Panicln(err)
	}

	app.Dao().ExpandRecord(record, []string{"user"}, nil)
	user := record.Expand()["user"].(*models.Record)
	user_name := user.GetString("name")
	title := record.GetString("title")
	body := fmt.Sprint("New announcement from ", user_name, ": ", title)

	response, err := client.Publish(
		&expo.PushMessage{
			To:    []expo.ExponentPushToken{pushToken},
			Body:  body,
			Title: title,
		},
	)

	if err != nil {
		log.Default().Panicln(err)
	}

	if response.ValidateResponse() != nil {
		fmt.Println(response.PushMessage.To, "failed")
	}
}

func main() {
	app := pocketbase.New()

	// serves static files from the provided public dir (if exists)
	app.OnBeforeServe().Add(func(e *core.ServeEvent) error {
		e.Router.GET("/*", apis.StaticDirectoryHandler(os.DirFS("./pb_public"), false))
		return nil
	})

	// add expiry cron job
	app.OnBeforeServe().Add(func(e *core.ServeEvent) error {
		scheduler := cron.New()

		// 0 0 * * * - every day at midnight
		scheduler.MustAdd("expiry", "0 0 * * *", func() {
			announcements, _ := app.Dao().FindRecordsByFilter("announcements", "calendar != \"\"", "-created", 0, 0)

			for _, v := range announcements {
				filename := v.GetString("calendar")
				url := "http://" + e.Server.Addr + "/api/files/announcements/" + v.Id + "/" + filename
				resp, _ := http.Get(url)
				data, _ := io.ReadAll(resp.Body)
				cal, _ := ics.ParseCalendar(strings.NewReader(string(data)))
				event := cal.Events()[0]
				end, _ := event.GetEndAt()

				if end.Before(time.Now()) {
					app.Dao().Delete(v)

					// remove from location ical
					f, _ := os.Open("./pb_public/" + v.GetString("location") + ".ics")
					f_data, _ := io.ReadAll(f)
					l_ical, _ := ics.ParseCalendar(strings.NewReader(string(f_data)))

					new_ical := ics.NewCalendar()
					new_ical.SetMethod(ics.MethodRequest)
					name := ""
					for _, v := range l_ical.CalendarProperties {
						v.IANAToken = "NAME"
						name = v.Value
					}

					new_ical.SetName(name)

					for _, v := range l_ical.Events() {
						if v.Id() != event.Id() {
							new_event := new_ical.AddEvent(v.Id())
							new_event.SetSummary(v.GetProperty("SUMMARY").Value)
							start, _ := v.GetStartAt()
							new_event.SetStartAt(start)
							end, _ := v.GetEndAt()
							new_event.SetEndAt(end)
							new_event.SetLocation(v.GetProperty("LOCATION").Value)
							break
						}
					}

					text := new_ical.Serialize()
					f.Write([]byte(text))
				}
			}
		})

		scheduler.Start()

		return nil
	})

	// Announcements notifications
	app.OnRecordAfterCreateRequest("announcements").Add(func(e *core.RecordCreateEvent) error {
		location := e.Record.GetString("location")

		if location == "" {
			devices, err := app.Dao().FindRecordsByFilter("devices", "token ~ \"ExponentPushToken\"", "-created", 0, 0, nil)

			if err != nil {
				log.Default().Panicln(err)
			}

			tokens := []string{}

			for _, v := range devices {
				tokens = append(tokens, v.GetString("token"))
			}

			for _, t := range tokens {
				announce(app, e.Record, t)
			}
		}

		users, err := app.Dao().FindRecordsByFilter("users", "location = {:location}", "-created", 0, 0, dbx.Params{"location": location})

		if err != nil {
			log.Default().Println(err)
		}

		tokens := []string{}

		for _, v := range users {
			app.Dao().ExpandRecord(v, []string{"devices"}, nil)

			devices := v.Expand()["devices"]

			if devices != nil {
				for _, v := range devices.([]*models.Record) {
					tokens = append(tokens, v.GetString("token"))
				}
			}
		}

		for _, t := range tokens {
			announce(app, e.Record, t)
		}

		return nil
	})

	// Creating channels for announcements
	app.OnRecordAfterCreateRequest("announcements").Add(func(e *core.RecordCreateEvent) error {
		channels_collection, _ := app.Dao().FindCollectionByNameOrId("channels")
		new_channel := models.NewRecord(channels_collection)
		new_channel.Set("isDefault", false)

		app.Dao().ExpandRecord(e.Record, []string{"location"}, nil)
		location := e.Record.Expand()["location"].(*models.Record)

		new_channel.Set("users", location.Get("leaders").([]string))
		new_channel.Set("announcement", e.Record.Id)
		new_channel.Set("title", "Announcement: \""+e.Record.GetString("title")+"\"")
		app.Dao().SaveRecord(new_channel)

		return nil
	})

	// Add ical to public dir
	app.OnRecordAfterCreateRequest("announcements").Add(func(e *core.RecordCreateEvent) error {
		location_file, _ := os.ReadFile("./pb_public/" + e.Record.GetString("location") + ".ics")
		location_cal, _ := ics.ParseCalendar(strings.NewReader(string(location_file)))

		announcement_files := e.UploadedFiles["calendar"]
		if len(announcement_files) == 0 {
			return nil
		}
		file := announcement_files[0]
		if file == nil {
			return nil
		}
		r, _ := file.Reader.Open()
		data, _ := io.ReadAll(r)
		cal_str := string(data)
		announcement_cal, _ := ics.ParseCalendar(strings.NewReader(cal_str))

		for _, v := range announcement_cal.Events() {
			new_event := location_cal.AddEvent(v.Id())
			new_event.SetSummary(v.GetProperty("SUMMARY").Value)
			start, _ := v.GetStartAt()
			new_event.SetStartAt(start)
			end, _ := v.GetEndAt()
			new_event.SetEndAt(end)
			new_event.SetLocation(v.GetProperty("LOCATION").Value)
		}

		text := location_cal.Serialize()

		f, _ := os.Create("./pb_public/" + e.Record.GetString("location") + ".ics")
		f.Write([]byte(text))

		return nil
	})

	app.OnRecordAfterUpdateRequest("announcements").Add(func(e *core.RecordUpdateEvent) error {
		location_file, _ := os.ReadFile("./pb_public/" + e.Record.GetString("location") + ".ics")
		location_cal, _ := ics.ParseCalendar(strings.NewReader(string(location_file)))

		announcement_files := e.UploadedFiles["calendar"]
		if len(announcement_files) == 0 {
			return nil
		}
		file := announcement_files[0]
		if file == nil {
			return nil
		}
		r, _ := file.Reader.Open()
		data, _ := io.ReadAll(r)
		cal_str := string(data)
		announcement_cal, _ := ics.ParseCalendar(strings.NewReader(cal_str))

		for _, v := range announcement_cal.Events() {
			new_event := location_cal.AddEvent(v.Id())
			new_event.SetSummary(v.GetProperty("SUMMARY").Value)
			start, _ := v.GetStartAt()
			new_event.SetStartAt(start)
			end, _ := v.GetEndAt()
			new_event.SetEndAt(end)
			new_event.SetLocation(v.GetProperty("LOCATION").Value)
		}

		text := location_cal.Serialize()

		f, _ := os.Create("./pb_public/" + e.Record.GetString("location") + ".ics")
		f.Write([]byte(text))

		return nil
	})

	// Push notifications for messages
	app.OnRecordAfterCreateRequest("messages").Add(func(e *core.RecordCreateEvent) error {
		app.Dao().ExpandRecord(e.Record, []string{"channel"}, nil)
		channel := e.Record.Expand()["channel"].(*models.Record)
		app.Dao().ExpandRecord(channel, []string{"users"}, nil)
		users := channel.Expand()["users"].([]*models.Record)

		tokens := []string{}

		for _, u := range users {
			if u.Id == e.Record.GetString("user") {
				continue
			}

			app.Dao().ExpandRecord(u, []string{"devices"}, nil)
			devices := u.Expand()["devices"]

			if devices != nil {
				for _, d := range devices.([]*models.Record) {
					tokens = append(tokens, d.GetString("token"))
				}
			}
		}

		client := expo.NewPushClient(nil)

		for _, t := range tokens {
			pushToken, err := expo.NewExponentPushToken(t)

			if err != nil {
				log.Default().Panicln(err)
			}

			app.Dao().ExpandRecord(e.Record, []string{"user"}, nil)
			user := e.Record.Expand()["user"].(*models.Record)
			user_name := user.GetString("name")
			title := fmt.Sprint("New announcement from ", user_name)
			body := e.Record.GetString("content")

			response, err := client.Publish(
				&expo.PushMessage{
					To:    []expo.ExponentPushToken{pushToken},
					Body:  body,
					Title: title,
				},
			)

			if err != nil {
				log.Default().Panicln(err)
			}

			if response.ValidateResponse() != nil {
				fmt.Println(response.PushMessage.To, "failed")
			}
		}

		return nil
	})

	// Creating channels for location leaders
	app.OnRecordAfterCreateRequest("locations").Add(func(e *core.RecordCreateEvent) error {
		channels_collection, _ := app.Dao().FindCollectionByNameOrId("channels")
		new_channel := models.NewRecord(channels_collection)
		new_channel.Set("isDefault", true)
		location_leaders := e.Record.Get("leaders").([]string)
		new_channel.Set("users", location_leaders)
		new_channel.Set("title", "Leaders of Back on Track "+e.Record.GetString("name"))
		app.Dao().SaveRecord(new_channel)

		return nil
	})

	// Create location ical
	app.OnRecordAfterCreateRequest("locations").Add(func(e *core.RecordCreateEvent) error {
		cal := ics.NewCalendar()
		cal.SetMethod(ics.MethodRequest)
		cal.SetName(e.Record.GetString("name"))

		text := cal.Serialize()

		f, _ := os.Create("./pb_public/" + e.Record.Id + ".ics")
		f.Write([]byte(text))

		return nil
	})

	// Location changing logic and channels
	app.OnRecordBeforeUpdateRequest("users").Add(func(e *core.RecordUpdateEvent) error {
		app.Dao().ExpandRecord(e.Record, []string{"location"}, nil)
		if e.Record.Expand()["location"] == nil {
			return nil
		}
		location := e.Record.Expand()["location"].(*models.Record)
		app.Dao().ExpandRecord(location, []string{"leaders"}, nil)
		is_leader := false
		leaders := [](*models.Record){}
		leaders_record, leaders_ok := location.Expand()["leaders"]
		if leaders_ok {
			leaders = leaders_record.([](*models.Record))
			for _, element := range leaders {
				if element.Id == e.Record.Id {
					is_leader = true
					break
				}
			}
		}

		old_location_id := e.Record.OriginalCopy().GetString("location")
		new_location_id := e.Record.GetString("location")

		location_changed := old_location_id != new_location_id
		old_location_leaders := []string{}
		old_location, old_location_ok := app.Dao().FindRecordById("locations", old_location_id)
		if old_location_ok == nil {
			old_location_leaders = old_location.Get("leaders").([]string)
		}

		was_leader := false
		for _, v := range old_location_leaders {
			if v == e.Record.Id {
				was_leader = true
				break
			}
		}

		if location_changed {
			if was_leader {
				// remove from old channels
				old_channels, _ := app.Dao().FindRecordsByFilter("channels", "(users ?~ {:user})", "-created", 0, 0, dbx.Params{"user": e.Record.Id})
				for _, v := range old_channels {
					channel := v
					users := channel.Get("users").([]string)
					new_users := []string{}
					for _, u := range users {
						if u != e.Record.Id {
							new_users = append(new_users, u)
						}
					}
					channel.Set("users", new_users)
					app.Dao().SaveRecord(channel)
				}
			} else {
				// delete old channel
				default_channel, _ := app.Dao().FindRecordsByFilter("channels", "(users ?~ {:user} && isDefault = True)", "-created", 0, 0, dbx.Params{"user": e.Record.Id})
				if len(default_channel) > 0 {
					channel := default_channel[0]
					app.Dao().Delete(channel)

				}
			}

			new_location, _ := app.Dao().FindRecordById("locations", new_location_id)
			if is_leader {
				// add to new channels
				new_channels, _ := app.Dao().FindRecordsByFilter("channels", "(location = {:location} && isDefault = True)", "-created", 0, 0, dbx.Params{"location": new_location_id})

				for _, v := range new_channels {
					channel := v
					users := channel.Get("users").([]string)
					users = append(users, e.Record.Id)
					channel.Set("users", users)
					app.Dao().SaveRecord(channel)
				}
			} else {
				// create new channel
				channels_collection, _ := app.Dao().FindCollectionByNameOrId("channels")
				channel := models.NewRecord(channels_collection)
				users := []string{e.Record.Id}
				leader_ids := []string{}
				for _, v := range leaders {
					if v.GetString("location") == new_location_id {
						leader_ids = append(leader_ids, v.Id)
					}
				}
				users = append(users, leader_ids...)
				channel.Set("users", users)
				channel.Set("isDefault", true)
				channel.Set("location", new_location_id)
				channel.Set("title", "Back on Track \""+new_location.GetString("name")+"\"")

				app.Dao().SaveRecord(channel)
			}
		}

		return nil
	})

	app.OnBeforeServe().Add(func(e *core.ServeEvent) error {
		e.Router.POST("/rsvp", func(c echo.Context) error {
			announcement_id := c.QueryParam("announcement_id")
			announcement_channels, _ := app.Dao().FindRecordsByFilter("channels", "announcement = {:announcement_id}", "-created", 0, 0, dbx.Params{"announcement_id": announcement_id})
			announcement_channel := announcement_channels[0]

			auth_record, _ := c.Get(apis.ContextAuthRecordKey).(*models.Record)

			if auth_record == nil {
				return c.JSON(http.StatusUnauthorized, map[string]interface{}{"status": "unauthorized"})
			}

			announcement_channel.Set("users", append(announcement_channel.Get("users").([]string), auth_record.Id))
			app.Dao().SaveRecord(announcement_channel)
			return c.JSON(http.StatusOK, map[string]interface{}{"status": "ok"})
		})

		return nil
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
