package interactive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/chroma/formatters"
	"github.com/alecthomas/chroma/lexers"
	"github.com/alecthomas/chroma/styles"
	"github.com/c-bata/go-prompt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/spf13/viper"
	"github.com/turbot/go-kit/helpers"
	"github.com/turbot/steampipe/pkg/cmdconfig"
	"github.com/turbot/steampipe/pkg/constants"
	"github.com/turbot/steampipe/pkg/db/db_common"
	"github.com/turbot/steampipe/pkg/db/db_local"
	"github.com/turbot/steampipe/pkg/display"
	"github.com/turbot/steampipe/pkg/error_helpers"
	"github.com/turbot/steampipe/pkg/query"
	"github.com/turbot/steampipe/pkg/query/metaquery"
	"github.com/turbot/steampipe/pkg/query/queryhistory"
	"github.com/turbot/steampipe/pkg/schema"
	"github.com/turbot/steampipe/pkg/statushooks"
	"github.com/turbot/steampipe/pkg/steampipeconfig"
	"github.com/turbot/steampipe/pkg/steampipeconfig/modconfig"
	"github.com/turbot/steampipe/pkg/utils"
	"github.com/turbot/steampipe/pkg/version"
)

type AfterPromptCloseAction int

const (
	AfterPromptCloseExit AfterPromptCloseAction = iota
	AfterPromptCloseRestart
)

// InteractiveClient is a wrapper over a LocalClient and a Prompt to facilitate interactive query prompt
type InteractiveClient struct {
	initData                *query.InitData
	promptResult            *RunInteractivePromptResult
	interactiveBuffer       []string
	interactivePrompt       *prompt.Prompt
	interactiveQueryHistory *queryhistory.QueryHistory
	autocompleteOnEmpty     bool
	// the cancellation function for the active query - may be nil
	// NOTE: should ONLY be called by cancelActiveQueryIfAny
	cancelActiveQuery context.CancelFunc
	cancelPrompt      context.CancelFunc
	// this cancellation is used to stop the pg notification listener which
	// we use to get connection config updates from the plugin manager
	// this is tied to a context which remaing valid throughout the life of the
	// interactive session
	cancelNotificationListener context.CancelFunc

	// channel used internally to pass the initialisation result
	initResultChan chan *db_common.InitResult
	// flag set when initialisation is complete (with or without errors)
	initialisationComplete bool
	afterClose             AfterPromptCloseAction
	// lock while execution is occurring to avoid errors/warnings being shown
	executionLock sync.Mutex
	// the schema metadata - this is loaded asynchronously during init
	schemaMetadata *schema.Metadata
	highlighter    *Highlighter
	// hidePrompt is used to render a blank as the prompt prefix
	hidePrompt bool

	querySuggestions []prompt.Suggest
	tableSuggestions []prompt.Suggest
}

func getHighlighter(theme string) *Highlighter {
	return newHighlighter(
		lexers.Get("sql"),
		formatters.Get("terminal256"),
		styles.Native,
	)
}

func newInteractiveClient(ctx context.Context, initData *query.InitData, result *RunInteractivePromptResult) (*InteractiveClient, error) {
	interactiveQueryHistory, err := queryhistory.New()
	if err != nil {
		return nil, err
	}
	c := &InteractiveClient{
		initData:                initData,
		promptResult:            result,
		interactiveQueryHistory: interactiveQueryHistory,
		interactiveBuffer:       []string{},
		autocompleteOnEmpty:     false,
		initResultChan:          make(chan *db_common.InitResult, 1),
		highlighter:             getHighlighter(viper.GetString(constants.ArgTheme)),
	}

	// asynchronously wait for init to complete
	// we start this immediately rather than lazy loading as we want to handle errors asap
	go c.readInitDataStream(ctx)

	return c, nil
}

// InteractivePrompt starts an interactive prompt and return
func (c *InteractiveClient) InteractivePrompt(parentContext context.Context) {
	// start a cancel handler for the interactive client - this will call activeQueryCancelFunc if it is set
	// (registered when we call createQueryContext)
	quitChannel := c.startCancelHandler()

	// create a cancel context for the prompt - this will set c.cancelPrompt
	ctx := c.createPromptContext(parentContext)

	defer func() {
		if r := recover(); r != nil {
			error_helpers.ShowError(ctx, helpers.ToError(r))
		}
		// close up the SIGINT channel so that the receiver goroutine can quit
		quitChannel <- true
		close(quitChannel)

		// cleanup the init data to ensure any services we started are stopped
		c.initData.Cleanup(ctx)

		// close the result stream
		// this needs to be the last thing we do,
		// as the query result display code will exit once the result stream is closed
		c.promptResult.Streamer.Close()
	}()

	statushooks.Message(
		ctx,
		fmt.Sprintf("Welcome to Steampipe v%s", version.SteampipeVersion.String()),
		fmt.Sprintf("For more information, type %s", constants.Bold(".help")),
	)

	// run the prompt in a goroutine, so we can also detect async initialisation errors
	promptResultChan := make(chan utils.InteractiveExitStatus, 1)
	c.runInteractivePromptAsync(ctx, &promptResultChan)

	// select results
	for {
		select {
		case initResult := <-c.initResultChan:
			c.handleInitResult(ctx, initResult)
			// if there was an error, handleInitResult will shut down the prompt
			// - we must wait for it to shut down and not return immediately

		case <-promptResultChan:
			// persist saved history
			c.interactiveQueryHistory.Persist()
			// check post-close action
			if c.afterClose == AfterPromptCloseExit {
				// clear prompt so any messages/warnings can be displayed without the prompt
				c.hidePrompt = true
				c.interactivePrompt.ClearLine()

				// stop the notification listener
				if c.cancelNotificationListener != nil {
					c.cancelNotificationListener()
				}
				return
			}
			// create new context with a cancellation func
			ctx = c.createPromptContext(parentContext)
			// now run it again
			c.runInteractivePromptAsync(ctx, &promptResultChan)
		}
	}
}

// ClosePrompt cancels the running prompt, setting the action to take after close
func (c *InteractiveClient) ClosePrompt(afterClose AfterPromptCloseAction) {
	c.afterClose = afterClose
	c.cancelPrompt()
}

// retrieve both the raw query result and a sanitised version in list form
func (c *InteractiveClient) loadSchema() error {
	utils.LogTime("db_client.loadSchema start")
	defer utils.LogTime("db_client.loadSchema end")

	// load these schemas
	// in a background context, since we are not running in a context - but GetSchemaFromDB needs one
	metadata, err := c.client().GetSchemaFromDB(context.Background())
	if err != nil {
		return fmt.Errorf("failed to load schemas: %s", err.Error())
	}

	c.schemaMetadata = metadata
	return nil
}

func (c *InteractiveClient) runInteractivePromptAsync(ctx context.Context, promptResultChan *chan utils.InteractiveExitStatus) {
	go func() {
		*promptResultChan <- c.runInteractivePrompt(ctx)
	}()
}

func (c *InteractiveClient) runInteractivePrompt(ctx context.Context) (ret utils.InteractiveExitStatus) {
	defer func() {
		// this is to catch the PANIC that gets raised by
		// the executor of go-prompt
		//
		// We need to do it this way, since there is no
		// clean way to reload go-prompt so that we can
		// populate the history stack
		//
		r := recover()
		switch v := r.(type) {
		case utils.InteractiveExitStatus:
			// this is a planned exit
			// set the return value
			ret = v
		default:
			if r != nil {
				// for everything else, float up the panic
				panic(r)
			}
		}
	}()

	callExecutor := func(line string) {
		c.executor(ctx, line)
	}
	completer := func(d prompt.Document) []prompt.Suggest {
		return c.queryCompleter(d)
	}
	c.interactivePrompt = prompt.New(
		callExecutor,
		completer,
		prompt.OptionTitle("steampipe interactive client "),
		prompt.OptionLivePrefix(func() (prefix string, useLive bool) {
			prefix = "> "
			useLive = true
			if len(c.interactiveBuffer) > 0 {
				prefix = ">>  "
			}
			if c.hidePrompt {
				prefix = ""
			}
			return
		}),
		prompt.OptionFormatter(c.highlighter.Highlight),
		prompt.OptionHistory(c.interactiveQueryHistory.Get()),
		prompt.OptionInputTextColor(prompt.DefaultColor),
		prompt.OptionPrefixTextColor(prompt.DefaultColor),
		prompt.OptionMaxSuggestion(20),
		// Known Key Bindings
		prompt.OptionAddKeyBind(prompt.KeyBind{
			Key: prompt.ControlC,
			Fn:  func(b *prompt.Buffer) { c.breakMultilinePrompt(b) },
		}),
		prompt.OptionAddKeyBind(prompt.KeyBind{
			Key: prompt.ControlD,
			Fn: func(b *prompt.Buffer) {
				if b.Text() == "" {
					c.ClosePrompt(AfterPromptCloseExit)
				}
			},
		}),
		prompt.OptionAddKeyBind(prompt.KeyBind{
			Key: prompt.Tab,
			Fn: func(b *prompt.Buffer) {
				if len(b.Text()) == 0 {
					c.autocompleteOnEmpty = true
				} else {
					c.autocompleteOnEmpty = false
				}
			},
		}),
		prompt.OptionAddKeyBind(prompt.KeyBind{
			Key: prompt.Escape,
			Fn: func(b *prompt.Buffer) {
				if len(b.Text()) == 0 {
					c.autocompleteOnEmpty = false
				}
			},
		}),
		prompt.OptionAddKeyBind(prompt.KeyBind{
			Key: prompt.ShiftLeft,
			Fn:  prompt.GoLeftChar,
		}),
		prompt.OptionAddKeyBind(prompt.KeyBind{
			Key: prompt.ShiftRight,
			Fn:  prompt.GoRightChar,
		}),
		prompt.OptionAddKeyBind(prompt.KeyBind{
			Key: prompt.ShiftUp,
			Fn:  func(b *prompt.Buffer) { /*ignore*/ },
		}),
		prompt.OptionAddKeyBind(prompt.KeyBind{
			Key: prompt.ShiftDown,
			Fn:  func(b *prompt.Buffer) { /*ignore*/ },
		}),
		// Opt+LeftArrow
		prompt.OptionAddASCIICodeBind(prompt.ASCIICodeBind{
			ASCIICode: constants.OptLeftArrowASCIICode,
			Fn:        prompt.GoLeftWord,
		}),
		// Opt+RightArrow
		prompt.OptionAddASCIICodeBind(prompt.ASCIICodeBind{
			ASCIICode: constants.OptRightArrowASCIICode,
			Fn:        prompt.GoRightWord,
		}),
		// Alt+LeftArrow
		prompt.OptionAddASCIICodeBind(prompt.ASCIICodeBind{
			ASCIICode: constants.AltLeftArrowASCIICode,
			Fn:        prompt.GoLeftWord,
		}),
		// Alt+RightArrow
		prompt.OptionAddASCIICodeBind(prompt.ASCIICodeBind{
			ASCIICode: constants.AltRightArrowASCIICode,
			Fn:        prompt.GoRightWord,
		}),
		prompt.OptionBufferPreHook(func(input string) (modifiedInput string, ignore bool) {
			// if this is not WSL, return as-is
			if !utils.IsWSL() {
				return input, false
			}
			return cleanBufferForWSL(input)
		}),
	)
	// set this to a default
	c.autocompleteOnEmpty = false
	c.interactivePrompt.RunCtx(ctx)

	return
}

func cleanBufferForWSL(s string) (string, bool) {
	b := []byte(s)
	// in WSL, 'Alt' combo-characters are denoted by [27, ASCII of character]
	// if we get a combination which has 27 as prefix - we should ignore it
	// this is inline with other interactive clients like pgcli
	if len(b) > 1 && bytes.HasPrefix(b, []byte{byte(27)}) {
		// ignore it
		return "", true
	}
	return string(b), false
}

func (c *InteractiveClient) breakMultilinePrompt(buffer *prompt.Buffer) {
	c.interactiveBuffer = []string{}
}

func (c *InteractiveClient) executor(ctx context.Context, line string) {
	// take an execution lock, so that errors and warnings don't show up while
	// we are underway
	c.executionLock.Lock()
	defer c.executionLock.Unlock()

	// set afterClose to restart - is we are exiting the metaquery will set this to AfterPromptCloseExit
	c.afterClose = AfterPromptCloseRestart

	line = strings.TrimSpace(line)

	resolvedQuery := c.getQuery(ctx, line)
	if resolvedQuery == nil {
		// we failed to resolve a query, or are in the middle of a multi-line entry
		// restart the prompt, DO NOT clear the interactive buffer
		c.restartInteractiveSession()
		return
	}

	// we successfully retrieved a query

	// create a  context for the execution of the query
	queryCtx := c.createQueryContext(ctx)

	if metaquery.IsMetaQuery(resolvedQuery.ExecuteSQL) {
		c.hidePrompt = true
		c.interactivePrompt.Render()

		if err := c.executeMetaquery(queryCtx, resolvedQuery.ExecuteSQL); err != nil {
			error_helpers.ShowError(ctx, err)
		}
		c.hidePrompt = false

		// cancel the context
		c.cancelActiveQueryIfAny()

	} else {
		statushooks.Show(ctx)
		defer statushooks.Done(ctx)

		// otherwise execute query
		t := time.Now()
		result, err := c.client().Execute(queryCtx, resolvedQuery.ExecuteSQL, resolvedQuery.Args...)
		if err != nil {
			error_helpers.ShowError(ctx, error_helpers.HandleCancelError(err))
			// if timing flag is enabled, show the time taken for the query to fail
			if cmdconfig.Viper().GetBool(constants.ArgTiming) {
				display.DisplayErrorTiming(t)
			}
		} else {
			c.promptResult.Streamer.StreamResult(result)
		}
	}

	// restart the prompt
	c.restartInteractiveSession()
}

func (c *InteractiveClient) getQuery(ctx context.Context, line string) *modconfig.ResolvedQuery {
	// if it's an empty line, then we don't need to do anything
	if line == "" {
		return nil
	}

	// store the history (the raw line which was entered)
	historyEntry := line
	defer func() {
		if len(historyEntry) > 0 {
			// we want to store even if we fail to resolve a query
			c.interactiveQueryHistory.Push(historyEntry)
		}

	}()

	// wait for initialisation to complete so we can access the workspace
	if !c.isInitialised() {
		// create a context used purely to detect cancellation during initialisation
		// this will also set c.cancelActiveQuery
		queryCtx := c.createQueryContext(ctx)
		defer func() {
			// cancel this context
			c.cancelActiveQueryIfAny()
		}()

		// show the spinner here while we wait for initialization to complete
		statushooks.Show(ctx)
		// wait for client initialisation to complete
		err := c.waitForInitData(queryCtx)
		statushooks.Done(ctx)
		if err != nil {
			// clear history entry
			historyEntry = ""
			// clear the interactive buffer
			c.interactiveBuffer = nil
			// error will have been handled elsewhere
			return nil
		}
	}

	// push the current line into the buffer
	c.interactiveBuffer = append(c.interactiveBuffer, line)

	// expand the buffer out into 'query'
	queryString := strings.Join(c.interactiveBuffer, "\n")

	// in case of a named query call with params, parse the where clause
	resolvedQuery, queryProvider, err := c.workspace().ResolveQueryAndArgsFromSQLString(queryString)
	if err != nil {
		// if we fail to resolve:
		// - show error but do not return it so we  stay in the prompt
		// - do not clear history item - we want to store bad entry in history
		// - clear interactive buffer
		c.interactiveBuffer = nil
		error_helpers.ShowError(ctx, err)
		return nil
	}
	isNamedQuery := queryProvider != nil

	// should we execute?
	// we will NOT execute if we are in multiline mode, there is no semi-colon
	// and it is NOT a metaquery or a named query
	if !c.shouldExecute(queryString, isNamedQuery) {
		// is we are not executing, do not store history
		historyEntry = ""
		// do not clear interactive buffer
		return nil
	}

	// so we need to execute
	// clear the interactive buffer
	c.interactiveBuffer = nil

	// what are we executing?

	// if the line is ONLY a semicolon, do nothing and restart interactive session
	if strings.TrimSpace(resolvedQuery.ExecuteSQL) == ";" {
		// do not store in history
		historyEntry = ""
		c.restartInteractiveSession()
		return nil
	}
	// if this is a multiline query, update history entry
	if !isNamedQuery && len(strings.Split(resolvedQuery.ExecuteSQL, "\n")) > 1 {
		historyEntry = resolvedQuery.ExecuteSQL
	}

	return resolvedQuery
}

func (c *InteractiveClient) executeMetaquery(ctx context.Context, query string) error {
	// the client must be initialised to get here
	if !c.isInitialised() {
		panic("client is not initalised")
	}
	// validate the metaquery arguments
	validateResult := metaquery.Validate(query)
	if validateResult.Message != "" {
		fmt.Println(validateResult.Message)
	}
	if err := validateResult.Err; err != nil {
		return err
	}
	if !validateResult.ShouldRun {
		return nil
	}
	client := c.client()
	// validation passed, now we will run
	return metaquery.Handle(ctx, &metaquery.HandlerInput{
		Query:       query,
		Client:      client,
		Schema:      c.schemaMetadata,
		Connections: c.initData.ConnectionMap,
		Prompt:      c.interactivePrompt,
		ClosePrompt: func() { c.afterClose = AfterPromptCloseExit },
	})
}

func (c *InteractiveClient) restartInteractiveSession() {
	// restart the prompt
	c.ClosePrompt(c.afterClose)
}

func (c *InteractiveClient) shouldExecute(line string, namedQuery bool) bool {
	if namedQuery {
		// execute named queries with no ';' even in multiline mode
		return true
	}
	if !cmdconfig.Viper().GetBool(constants.ArgMultiLine) {
		// NOT multiline mode
		return true
	}
	if metaquery.IsMetaQuery(line) {
		// execute metaqueries with no ';' even in multiline mode
		return true
	}
	if strings.HasSuffix(line, ";") {
		// statement has terminating ';'
		return true
	}

	return false
}

func (c *InteractiveClient) queryCompleter(d prompt.Document) []prompt.Suggest {
	if !cmdconfig.Viper().GetBool(constants.ArgAutoComplete) {
		return nil
	}
	if !c.isInitialised() {
		return nil
	}

	text := strings.TrimLeft(strings.ToLower(d.CurrentLine()), " ")
	if len(text) == 0 && !c.autocompleteOnEmpty {
		// if nothing has been typed yet, no point
		// giving suggestions
		return nil
	}

	var s []prompt.Suggest

	switch {
	case isFirstWord(text):
		// add all we know that can be the first words
		// named queries
		s = append(s, c.querySuggestions...)
		// "select"
		s = append(s, prompt.Suggest{Text: "select", Output: "select"}, prompt.Suggest{Text: "with", Output: "with"})
		// metaqueries
		s = append(s, metaquery.PromptSuggestions()...)
	case metaquery.IsMetaQuery(text):
		suggestions := metaquery.Complete(&metaquery.CompleterInput{
			Query:            text,
			TableSuggestions: c.tableSuggestions,
		})
		s = append(s, suggestions...)
	default:
		if queryInfo := getQueryInfo(text); queryInfo.EditingTable {
			s = append(s, c.tableSuggestions...)
		}
	}

	return prompt.FilterHasPrefix(s, d.GetWordBeforeCursor(), true)
}

func (c *InteractiveClient) addSuggestion(itemType string, description string, name string) prompt.Suggest {
	if description != "" {
		itemType += fmt.Sprintf(": %s", description)
	}
	return prompt.Suggest{Text: name, Output: name, Description: itemType}
}

func (c *InteractiveClient) startCancelHandler() chan bool {
	sigIntChannel := make(chan os.Signal, 1)
	quitChannel := make(chan bool, 1)
	signal.Notify(sigIntChannel, os.Interrupt)
	go func() {
		for {
			select {
			case <-sigIntChannel:
				log.Println("[TRACE] interactive client cancel handler got SIGINT")
				// if initialisation is not complete, just close the prompt
				// this will cancel the context used for initialisation so cancel any initialisation queries
				if !c.isInitialised() {
					c.ClosePrompt(AfterPromptCloseExit)
					return
				} else {
					// otherwise call cancelActiveQueryIfAny which the for the active query, if there is one
					c.cancelActiveQueryIfAny()
					// keep waiting for further cancellations
				}
			case <-quitChannel:
				log.Println("[TRACE] cancel handler exiting")
				c.cancelActiveQueryIfAny()
				// we're done
				return
			}
		}
	}()
	return quitChannel
}

func (c *InteractiveClient) listenToPgNotifications(ctx context.Context) error {
	log.Printf("[TRACE] InteractiveClient listenToPgNotifications")
	conn, err := c.getNotificationConnection(ctx)
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		if err != nil {
			return err
		}

		log.Printf("[TRACE] Wait for notification")
		notification, err := conn.WaitForNotification(ctx)
		if err != nil && !error_helpers.IsContextCancelledError(err) {
			log.Printf("[INFO] Error waiting for notification: %s", err)
		}

		if notification != nil {
			c.handlePostgresNotification(ctx, notification)
		}
		log.Printf("[TRACE] Handled notification")
	}
	conn.Close(ctx)

	log.Printf("[TRACE] InteractiveClient listenToPgNotifications DONE")
	return nil
}

func (c *InteractiveClient) getNotificationConnection(ctx context.Context) (*pgx.Conn, error) {
	conn, err := db_local.CreateLocalDbConnection(ctx, &db_local.CreateDbOptions{Username: constants.DatabaseUser})
	if err != nil {
		return nil, err
	}

	listenSql := fmt.Sprintf("listen %s", constants.PostgresNotificationChannel)
	_, err = conn.Exec(ctx, listenSql)
	if err != nil {
		log.Printf("[INFO] Error listening to schema channel: %s", err)
		conn.Close(ctx)
		return nil, err
	}
	return conn, nil
}

func (c *InteractiveClient) handlePostgresNotification(ctx context.Context, notification *pgconn.Notification) {
	if notification == nil {
		return
	}
	log.Printf("[TRACE] handleConnectionUpdateNotification: %s", notification.Payload)
	n := &steampipeconfig.PostgresNotification{}
	err := json.Unmarshal([]byte(notification.Payload), n)
	if err != nil {
		log.Printf("[INFO] Error unmarshalling notification: %s", err)
		return
	}
	switch n.Type {
	case steampipeconfig.PgNotificationSchemaUpdate:
		// unmarshal the notification again, into the correct type
		schemaUpdateNotification := &steampipeconfig.SchemaUpdateNotification{}
		if err := json.Unmarshal([]byte(notification.Payload), schemaUpdateNotification); err != nil {
			log.Printf("[INFO] Error unmarshalling notification: %s", err)
			return
		}
		c.handleConnectionUpdateNotification(ctx, schemaUpdateNotification)
	}
}

func (c *InteractiveClient) handleConnectionUpdateNotification(ctx context.Context, notification *steampipeconfig.SchemaUpdateNotification) {
	// at present, we do not actually use the payload, we just do a brute force reload
	// as an optimization we could look at the updates and only reload the required schemas

	log.Printf("[TRACE] handleConnectionUpdateNotification")
	// reload the connection data map
	// first load foreign schema names
	if err := c.client().LoadSchemaNames(ctx); err != nil {
		log.Printf("[INFO] Error loading foreign schema names: %v", err)
	}
	// now reload state
	connectionMap, _, err := steampipeconfig.GetConnectionState(c.client().ForeignSchemaNames())
	if err != nil {
		log.Printf("[INFO] Error loading connection state: %v", err)
		return
	}
	// and save it
	c.initData.ConnectionMap = connectionMap

	// reload config before reloading schema
	config, errorsAndWarnings := steampipeconfig.LoadSteampipeConfig(viper.GetString(constants.ArgModLocation), "query")
	if errorsAndWarnings.GetError() != nil {
		log.Printf("[WARN] Error reloading config: %v", errorsAndWarnings.GetError())
		return
	}
	steampipeconfig.GlobalConfig = config

	// reload schema
	if err := c.loadSchema(); err != nil {
		log.Printf("[INFO] Error unmarshalling notification: %s", err)
		return
	}
	// reinitialise autocomplete suggestions
	c.initialiseSuggestions()

	// refresh the db session inside an execution lock
	// we do this to avoid the postgres `cached plan must not change result type`` error
	c.executionLock.Lock()
	defer c.executionLock.Unlock()

	c.client().RefreshSessions(ctx)
	log.Printf("[TRACE] completed refresh session")
}
