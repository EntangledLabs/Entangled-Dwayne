package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sync"
	"time"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	"github.com/BurntSushi/toml"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const (
	defaultDelay  = 60
	defaultJitter = 30
	apiEndpoint   = "http://172.16.1.122"
)

type injectList struct {
	Inject []Inject
}

var (
	dwConf = &config{}
	db     = &gorm.DB{}

	roundNumber int
	ct          CredentialTable
	loc *time.Location

	teamMutex    = &sync.Mutex{}
	persistMutex = &sync.Mutex{}
)

func errorPrint(a ...interface{}) {
	log.Printf("[ERROR] %s", fmt.Sprintln(a...))
}

func debugPrint(a ...interface{}) {
	log.Printf("[DEBUG] %s", fmt.Sprintln(a...))
}

func main() {
	readConfig(dwConf)
	err := checkConfig(dwConf)
	if err != nil {
		log.Fatalln(errors.Wrap(err, "illegal config"))
	}

	// Hardcoded CST timezone
	loc, err = time.LoadLocation(dwConf.Timezone)
	if err != nil {
		log.Fatalln(errors.Wrap(err, "invalid timezone"))
	}

	// Open database
	db, err = gorm.Open(sqlite.Open("dwayne.db"), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect database!")
	}

	db.AutoMigrate(&ResultEntry{}, &TeamRecord{}, &Inject{}, &InjectSubmission{}, &TeamData{}, &SLA{}, &Persist{})

	if dwConf.Persists {
		persistHits = make(map[uint]map[string][]uint)
	}

	// Assign team IDs
	for i := range dwConf.Team {
		dwConf.Team[i].ID = uint(i + 1)
	}

	// Save into DB if not already in there.
	var teams []TeamData
	res := db.Find(&teams)
	if res.Error == nil && len(teams) == 0 {
		for _, team := range dwConf.Team {
			if res := db.Create(&team); res.Error != nil {
				log.Fatal("unable to save team in database")
			}
		}
	}

	// Initialize mutex for credential table
	ct.Mutex = &sync.Mutex{}

	// Initialize Gin router
	// gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// Add... add function
	r.SetFuncMap(template.FuncMap{
		"increment": func(x int) int {
			return x + 1
		},
	})

	r.LoadHTMLGlob("templates/*")
	r.Static("/assets", "./assets")
	initCookies(r)

	// 404 handler
	r.NoRoute(func(c *gin.Context) {
		c.HTML(http.StatusNotFound, "404.html", pageData(c, "Login", nil))
	})

	// Routes
	routes := r.Group("/")
	{
		routes.GET("/", viewStatus)
		routes.GET("/info", func(c *gin.Context) {
			c.HTML(http.StatusOK, "info.html", pageData(c, "Information", nil))
		})
		routes.GET("/login", func(c *gin.Context) {
			if getUserOptional(c).IsValid() {
				c.Redirect(http.StatusSeeOther, "/")
			}
			c.HTML(http.StatusOK, "login.html", pageData(c, "Login", nil))
		})
		routes.GET("/forbidden", func(c *gin.Context) {
			c.HTML(http.StatusOK, "forbidden.html", pageData(c, "Forbidden", nil))
		})
		routes.POST("/login", login)
		if dwConf.Persists {
			routes.GET("/persist/:token", scorePersist)
			routes.GET("/persist", viewPersist)
		}

		// Has API key check. If more API routes are added in the future,
		// add own endpoint group with auth middleware
		routes.POST("/injects", createInject)
	}

	authRoutes := routes.Group("/")
	authRoutes.Use(authRequired)
	{
		authRoutes.GET("/logout", logout)

		// Team Information
		authRoutes.GET("/export/:team", exportTeamData)
		authRoutes.GET("/team/:team", viewTeam)
		authRoutes.GET("/team/:team/:check", viewCheck)
		authRoutes.GET("/uptime/:team", viewUptime)

		// PCRs
		authRoutes.GET("/pcr", viewPCR)
		if dwConf.EasyPCR {
			authRoutes.POST("/pcr", submitPCR)
		}

		// Red Team
		authRoutes.GET("/red", viewRed)
		authRoutes.POST("/red", submitRed)

		// Injects
		authRoutes.GET("/injects", viewInjects)
		authRoutes.GET("/injects/feed", injectFeed)
		authRoutes.GET("/injects/new", func(c *gin.Context) {
			c.HTML(http.StatusOK, "new_inject.html", pageData(c, "New Inject", nil))
		})
		authRoutes.POST("/injects/new", newInject)
		authRoutes.GET("/injects/view/:inject", viewInject)
		authRoutes.POST("/injects/view/:inject", submitInject)
		authRoutes.POST("/injects/view/:inject/:submission/invalid", invalidateInject)
		authRoutes.GET("/injects/view/:inject/:submission/grade", gradeInject)
		authRoutes.POST("/injects/view/:inject/:submission/grade", submitInjectGrade)

		// Inject submissions
		authRoutes.Static("/submissions", "./submissions")

		// Settings
		authRoutes.GET("/settings", viewSettings)

		// Resets
		authRoutes.GET("/reset", viewResets)
		authRoutes.POST("/reset/:id", submitReset)
	}

	var injects []Inject
	res = db.Find(&injects)
	if res.Error != nil {
		errorPrint(res.Error)
		return
	}

	if len(injects) == 0 {
		if !dwConf.NoPasswords {
			pwChangeInject := Inject{
				Time:  time.Now(),
				Title: "Password Changes",
				Body:  "Submit your password changes here!",
			}
			res := db.Create(&pwChangeInject)
			if res.Error != nil {
				errorPrint(res.Error)
				return
			}
		}

		var configInjects injectList

		fileContent, err := os.ReadFile("./injects.conf")
		if err != nil {
			debugPrint("Configuration file ("+configPath+") not found:", err)
		} else {
			if _, err := toml.Decode(string(fileContent), &configInjects); err != nil {
				log.Fatalln(err)
			}

			for _, inject := range configInjects.Inject {
				res := db.Create(&inject)
				if res.Error != nil {
					errorPrint(res.Error)
					return
				}
			}

		}
	} else {
		debugPrint("Injects list is not empty, so we are not adding password change inject or processing configured injects")
	}

	go Score(dwConf)
	r.Run(":80")
}
