package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
)

/* --------------------------------------------------------------------------
   ERRORS
-------------------------------------------------------------------------- */

var ErrEmptyInput = errors.New("input cannot be empty")
var ErrInvalidSelection = errors.New("invalid selection")
var ErrInvalidUserCount = errors.New("invalid user count")

// InputError carries contextual information about a bad user input.
type InputError struct {
	Value string
	Code  int
}

func (e InputError) Error() string {
	return fmt.Sprintf("ID: %d | User typed: '%s' (Invalid)", e.Code, e.Value)
}

/* --------------------------------------------------------------------------
   STRUCTS
-------------------------------------------------------------------------- */

// Question represents a single quiz question with shuffled options.
type Question struct {
	Text          string   `json:"Text"`
	Options       []string `json:"Options"`
	CorrectAnswer int      `json:"CorrectAnswer"`
}

// UserSession holds the name and live score for an active player.
type UserSession struct {
	Name  string `json:"name"`
	Score int    `json:"score"`
}

// AuthRequest is the JSON body for /api/register and /api/login.
type AuthRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// FinishRequest is the JSON body for /api/finish.
type FinishRequest struct {
	Users []UserSession `json:"users"`
}

/* --------------------------------------------------------------------------
   PERSISTENCE
-------------------------------------------------------------------------- */

// UserAccount is a persisted user record; Password stores the bcrypt hash.
type UserAccount struct {
	Username  string `json:"username"`
	Password  string `json:"password"` // bcrypt hash — never stored as plaintext
	HighScore int    `json:"high_score"`
}

// UserDBPath is the path to the flat-file JSON user database.
var UserDBPath = "users.json"

// userMu guards all reads and writes to the user database file so that
// concurrent HTTP requests cannot corrupt it.
var userMu sync.Mutex

// loadUsers reads all user accounts from disk (caller must hold userMu).
func loadUsers() ([]UserAccount, error) {
	data, err := os.ReadFile(UserDBPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []UserAccount{}, nil
		}
		return nil, err
	}
	var users []UserAccount
	err = json.Unmarshal(data, &users)
	return users, err
}

// saveUsers writes the user slice to disk (caller must hold userMu).
func saveUsers(users []UserAccount) error {
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(UserDBPath, data, 0644)
}

/* --------------------------------------------------------------------------
   CONCURRENCY — BACKGROUND SCORE SYNC
-------------------------------------------------------------------------- */

// syncScores sends a formatted sync-complete message on resultsChan.
// Designed to run as a goroutine; wg.Done() signals completion to the
// WaitGroup so the caller knows all goroutines have finished.
func syncScores(u UserSession, resultsChan chan<- string, wg *sync.WaitGroup) {
	defer wg.Done()
	// Simulate lightweight background processing (e.g., async analytics push)
	resultsChan <- fmt.Sprintf("✅ Background sync complete for %s with score %d", u.Name, u.Score)
}

/* --------------------------------------------------------------------------
   TRIVIA API — QUESTION FETCH
-------------------------------------------------------------------------- */

// APIQuestion mirrors the shape returned by the-trivia-api.com v2 endpoint.
type APIQuestion struct {
	Category         string   `json:"category"`
	CorrectAnswer    string   `json:"correctAnswer"`
	IncorrectAnswers []string `json:"incorrectAnswers"`
	Question         struct {
		Text string `json:"text"`
	} `json:"question"`
	Difficulty string `json:"difficulty"`
}

// quizData is the in-memory store: Field → Topic → []Question.
// Written once at startup (single goroutine) then only read.
var quizData map[string]map[string][]Question

// fetchTriviaQuestions fetches 50 questions from the public Trivia API,
// normalises them into our internal Question format, and stores them in
// the package-level quizData variable.
func fetchTriviaQuestions() error {
	resp, err := http.Get("https://the-trivia-api.com/v2/questions?limit=50&types=text_choice")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var apiQs []APIQuestion
	if err := json.NewDecoder(resp.Body).Decode(&apiQs); err != nil {
		return err
	}

	rand.Seed(time.Now().UnixNano()) //nolint:staticcheck

	data := make(map[string]map[string][]Question)

	for _, q := range apiQs {
		categoryStr := strings.ReplaceAll(q.Category, "_", " ")
		category := strings.Title(categoryStr) //nolint:staticcheck

		topic := strings.Title(q.Difficulty) //nolint:staticcheck
		if topic == "" {
			topic = "General"
		}

		if data[category] == nil {
			data[category] = make(map[string][]Question)
		}

		options := append([]string{}, q.IncorrectAnswers...)
		options = append(options, q.CorrectAnswer)

		rand.Shuffle(len(options), func(i, j int) {
			options[i], options[j] = options[j], options[i]
		})

		var correctIndex int
		for i, opt := range options {
			if opt == q.CorrectAnswer {
				correctIndex = i + 1
				break
			}
		}

		data[category][topic] = append(data[category][topic], Question{
			Text:          q.Question.Text,
			Options:       options,
			CorrectAnswer: correctIndex,
		})
	}

	quizData = data
	return nil
}

/* --------------------------------------------------------------------------
   MIDDLEWARE
-------------------------------------------------------------------------- */

// loggingMiddleware wraps a handler and logs the method, path, and elapsed
// time.  The log write is dispatched to a goroutine over a buffered channel
// so it never blocks the response path.
func loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	logCh := make(chan string, 64)

	// Background logger goroutine — drains the channel and writes to stdout.
	go func() {
		for msg := range logCh {
			log.Println(msg)
		}
	}()

	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next(w, r)
		elapsed := time.Since(start)
		logCh <- fmt.Sprintf("[%s] %s %s — %s", time.Now().Format("15:04:05"), r.Method, r.URL.Path, elapsed)
	}
}

// corsMiddleware adds permissive CORS headers so the API can be consumed
// by external front-ends during development.
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// chain applies middleware in left-to-right order (outermost first).
func chain(h http.HandlerFunc, middlewares ...func(http.HandlerFunc) http.HandlerFunc) http.HandlerFunc {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

/* --------------------------------------------------------------------------
   API HANDLERS
-------------------------------------------------------------------------- */

// registerHandler — POST /api/register
// Validates input, hashes the password with bcrypt (DefaultCost = 10),
// and persists the new account only if the username is unique.
func registerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req AuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Input validation and length limits
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		http.Error(w, "Username and password cannot be empty", http.StatusBadRequest)
		return
	}
	if len(req.Username) > 32 || len(req.Password) > 128 {
		http.Error(w, "Username or password exceeds maximum length", http.StatusBadRequest)
		return
	}

	userMu.Lock()
	defer userMu.Unlock()

	users, err := loadUsers()
	if err != nil {
		http.Error(w, "Error loading users database", http.StatusInternalServerError)
		return
	}

	for _, u := range users {
		if u.Username == req.Username {
			http.Error(w, "Username already exists", http.StatusConflict)
			return
		}
	}

	// bcrypt.DefaultCost = 10 — balances security and latency
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Error hashing password", http.StatusInternalServerError)
		return
	}

	users = append(users, UserAccount{Username: req.Username, Password: string(hash)})
	if err := saveUsers(users); err != nil {
		http.Error(w, "Error saving user", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"message": "Registration successful"})
}

// loginHandler — POST /api/login
// Compares the supplied password against the stored bcrypt hash using
// constant-time comparison to prevent timing-based enumeration attacks.
func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req AuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req.Username = strings.TrimSpace(req.Username)

	userMu.Lock()
	defer userMu.Unlock()

	users, err := loadUsers()
	if err != nil {
		http.Error(w, "Error loading users database", http.StatusInternalServerError)
		return
	}

	for _, u := range users {
		if u.Username == req.Username {
			// bcrypt.CompareHashAndPassword is constant-time — safe against timing attacks
			if err := bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(req.Password)); err == nil {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(UserSession{Name: req.Username, Score: 0})
				return
			}
			http.Error(w, "Invalid password", http.StatusUnauthorized)
			return
		}
	}

	http.Error(w, "User not found", http.StatusNotFound)
}

// quizHandler — GET /api/quiz
// Returns the full in-memory quiz catalogue as JSON.
func quizHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(quizData)
}

// statsHandler — GET /api/stats
// Returns the current global high score across all registered users.
func statsHandler(w http.ResponseWriter, r *http.Request) {
	userMu.Lock()
	defer userMu.Unlock()

	users, err := loadUsers()
	if err != nil {
		http.Error(w, "Error loading users database", http.StatusInternalServerError)
		return
	}

	maxScore := 0
	for _, u := range users {
		if u.HighScore > maxScore {
			maxScore = u.HighScore
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"globalHighScore": maxScore,
	})
}

/* --------------------------------------------------------------------------
   LEADERBOARD
-------------------------------------------------------------------------- */

// LeaderboardEntry is a public-safe view of a user account for the leaderboard.
type LeaderboardEntry struct {
	Rank      int    `json:"rank"`
	Username  string `json:"username"`
	HighScore int    `json:"highScore"`
}

// leaderboardHandler — GET /api/leaderboard
// Returns all users sorted by high score descending with ordinal ranks.
func leaderboardHandler(w http.ResponseWriter, r *http.Request) {
	userMu.Lock()
	defer userMu.Unlock()

	users, err := loadUsers()
	if err != nil {
		http.Error(w, "Error loading users database", http.StatusInternalServerError)
		return
	}

	// Sort descending by high score
	sort.Slice(users, func(i, j int) bool {
		return users[i].HighScore > users[j].HighScore
	})

	entries := make([]LeaderboardEntry, len(users))
	for i, u := range users {
		entries[i] = LeaderboardEntry{
			Rank:      i + 1,
			Username:  u.Username,
			HighScore: u.HighScore,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// finishHandler — POST /api/finish
// Sorts session scores, runs background sync goroutines with a WaitGroup,
// and updates each user's persistent high score if it was beaten.
func finishHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req FinishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	allUsers := req.Users

	// Sort session ranking globally
	sort.Slice(allUsers, func(i, j int) bool {
		return allUsers[i].Score > allUsers[j].Score
	})

	log.Println("\n--- FINAL RANKINGS (API) ---")
	for i, u := range allUsers {
		log.Printf("%d. %s - %d pts", i+1, u.Name, u.Score)
	}
	if len(allUsers) > 0 {
		log.Printf("🏆 Winner: %s with %d pts!", allUsers[0].Name, allUsers[0].Score)
	}

	// Concurrency: fan-out sync goroutines, collect via channel + WaitGroup
	log.Println("--- Background Score Sync (goroutines + WaitGroup) ---")
	resultsChan := make(chan string, len(allUsers))
	var wg sync.WaitGroup

	for _, u := range allUsers {
		wg.Add(1)
		go syncScores(u, resultsChan, &wg)
	}

	// Close the channel once ALL goroutines have called wg.Done()
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Drain results — loop ends naturally when channel is closed
	for msg := range resultsChan {
		log.Println(msg)
	}

	// Update persistent high scores (mutex-safe)
	userMu.Lock()
	dbUsers, err := loadUsers()
	if err == nil {
		updated := false
		for _, sessionUser := range allUsers {
			for i, dbUser := range dbUsers {
				if dbUser.Username == sessionUser.Name {
					if sessionUser.Score > dbUser.HighScore {
						dbUsers[i].HighScore = sessionUser.Score
						updated = true
					}
					break
				}
			}
		}
		if updated {
			saveUsers(dbUsers)
		}
	}
	userMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "Scores saved and synced successfully",
		"ranking": allUsers,
	})
}

/* --------------------------------------------------------------------------
   EMBEDDED UI
-------------------------------------------------------------------------- */

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Multi-User Quiz App</title>
    <script src="https://unpkg.com/react@18/umd/react.production.min.js" crossorigin></script>
    <script src="https://unpkg.com/react-dom@18/umd/react-dom.production.min.js" crossorigin></script>
    <script src="https://unpkg.com/@babel/standalone/babel.min.js"></script>
    <script src="https://cdn.tailwindcss.com"></script>
    <link href="https://fonts.googleapis.com/css2?family=Inter:wght@300;400;500;600;700;900&display=swap" rel="stylesheet">
    <script src="https://unpkg.com/feather-icons"></script>
    <style>
        body { font-family: 'Inter', sans-serif; background: #f8fafc; min-height: 100vh; color: #111; }
        ::-webkit-scrollbar { width: 6px; }
        ::-webkit-scrollbar-track { background: transparent; }
        ::-webkit-scrollbar-thumb { background: rgba(0,0,0,0.1); border-radius: 4px; }
        @keyframes fadeInUp {
            from { opacity: 0; transform: translateY(24px); }
            to   { opacity: 1; transform: translateY(0); }
        }
        .fade-in { animation: fadeInUp 0.45s cubic-bezier(0.16,1,0.3,1) forwards; }
        @keyframes spin { to { transform: rotate(360deg); } }
        .spinner { animation: spin 0.8s linear infinite; }
        @keyframes pulse-ring {
            0%   { transform: scale(0.95); box-shadow: 0 0 0 0 rgba(59,130,246,0.4); }
            70%  { transform: scale(1);    box-shadow: 0 0 0 12px rgba(59,130,246,0); }
            100% { transform: scale(0.95); box-shadow: 0 0 0 0 rgba(59,130,246,0); }
        }
        .pulse { animation: pulse-ring 2s infinite; }
    </style>
</head>
<body class="flex justify-center items-start min-h-screen w-screen overflow-x-hidden bg-slate-50 antialiased">

<div id="root" class="w-full flex justify-center items-start min-h-screen"></div>

<script type="text/babel">
    const { useState, useEffect, useCallback } = React;

    /* ── Reusable UI Primitives ── */

    const Button = ({ children, onClick, variant = 'primary', className = '', disabled = false }) => {
        const base = "w-full py-[14px] rounded-2xl font-bold text-base transition-all duration-200 active:scale-[0.97] disabled:opacity-40 disabled:cursor-not-allowed";
        const variants = {
            primary: "bg-blue-600 text-white shadow-[0_6px_18px_rgba(37,99,235,0.28)] hover:bg-blue-700 hover:shadow-[0_8px_24px_rgba(37,99,235,0.36)]",
            secondary: "bg-white border border-gray-200 text-gray-700 hover:bg-gray-50 hover:border-gray-300",
            outline:   "border-2 border-gray-200 text-gray-600 bg-transparent hover:bg-gray-50",
            danger:    "bg-red-500 text-white hover:bg-red-600",
        };
        return (
            <button disabled={disabled} onClick={onClick} className={base + ' ' + variants[variant] + ' ' + className}>
                {children}
            </button>
        );
    };

    const Input = ({ type = "text", placeholder, value, onChange, onKeyDown }) => (
        <input
            type={type}
            placeholder={placeholder}
            value={value}
            onChange={onChange}
            onKeyDown={onKeyDown}
            className="w-full px-4 py-3.5 rounded-xl bg-gray-50 border border-gray-200 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:bg-white transition-all text-gray-800 placeholder-gray-400 font-medium"
        />
    );

    const Badge = ({ children, color = 'blue' }) => {
        const colors = { blue: 'bg-blue-50 text-blue-700', green: 'bg-green-50 text-green-700', yellow: 'bg-amber-50 text-amber-700', gray: 'bg-gray-100 text-gray-500' };
        return <span className={"text-xs font-bold px-3 py-1 rounded-full uppercase tracking-widest " + colors[color]}>{children}</span>;
    };

    const ProgressBar = ({ value, max }) => {
        const pct = max > 0 ? Math.round((value / max) * 100) : 0;
        return (
            <div className="w-full mb-6">
                <div className="flex justify-between text-xs font-bold text-blue-600 uppercase tracking-widest mb-2">
                    <span>Q {value} / {max}</span>
                    <span>{pct}%</span>
                </div>
                <div className="w-full h-2.5 bg-gray-100 rounded-full overflow-hidden">
                    <div className="h-full bg-gradient-to-r from-blue-500 to-indigo-500 rounded-full transition-all duration-500" style={{ width: pct + '%' }}/>
                </div>
            </div>
        );
    };

    const Card = ({ children, className = '' }) => (
        <div className={"bg-white border border-gray-100 rounded-2xl shadow-[0_4px_20px_rgba(0,0,0,0.05)] " + className}>
            {children}
        </div>
    );

    /* ── Main App ── */
    const App = () => {
        const [step, setStep]                     = useState('setup');
        const [totalUsers, setTotalUsers]         = useState(2);
        const [activeSessions, setActiveSessions] = useState([]);
        const [currentUserIndex, setCurrentUserIndex] = useState(0);
        const [quizData, setQuizData]             = useState(null);
        const [completedTopics, setCompletedTopics] = useState({});
        const [selectedField, setSelectedField]   = useState(null);
        const [selectedTopic, setSelectedTopic]   = useState(null);
        const [loadingQuiz, setLoadingQuiz]       = useState(true);

        useEffect(() => {
            fetch('/api/quiz')
                .then(r => r.json())
                .then(data => { setQuizData(data); setLoadingQuiz(false); })
                .catch(() => setLoadingQuiz(false));
        }, []);

        useEffect(() => { if (window.feather) feather.replace(); });

        const getTotalTopics = useCallback(() => {
            if (!quizData) return 0;
            return Object.values(quizData).reduce((sum, field) => sum + Object.keys(field).length, 0);
        }, [quizData]);

        const handleSetup = (count) => {
            setTotalUsers(count);
            setStep('auth');
            setCurrentUserIndex(0);
            setActiveSessions([]);
        };

        const handleAuthSuccess = (session) => {
            const newSessions = [...activeSessions, session];
            setActiveSessions(newSessions);
            if (newSessions.length < totalUsers) {
                setCurrentUserIndex(newSessions.length);
            } else {
                setCurrentUserIndex(0);
                setCompletedTopics({});
                setStep('dashboard');
            }
        };

        const handleTopicFinished = (pointsGained) => {
            const newSessions = [...activeSessions];
            newSessions[currentUserIndex].score += pointsGained;
            setActiveSessions(newSessions);

            const topicKey = selectedField + ':' + selectedTopic;
            const newCompleted = { ...completedTopics, [topicKey]: true };
            setCompletedTopics(newCompleted);

            if (Object.keys(newCompleted).length >= getTotalTopics()) {
                if (currentUserIndex + 1 < totalUsers) {
                    setCurrentUserIndex(currentUserIndex + 1);
                    setCompletedTopics({});
                    setStep('dashboard');
                } else {
                    setStep('results');
                }
            } else {
                setStep('dashboard');
            }
        };

        const handleSkipTurn = () => {
            if (currentUserIndex + 1 < totalUsers) {
                setCurrentUserIndex(currentUserIndex + 1);
                setCompletedTopics({});
                setStep('dashboard');
            } else {
                setStep('results');
            }
        };

        /* ── Setup Screen ── */
        const SetupScreen = () => {
            const [count, setCount] = useState(2);
            return (
                <div className="flex flex-col items-center justify-center min-h-screen p-6 fade-in">
                    <Card className="w-full max-w-sm p-8">
                        <div className="w-20 h-20 rounded-2xl bg-gradient-to-br from-blue-500 to-indigo-600 flex items-center justify-center shadow-lg mx-auto mb-6 pulse">
                            <i data-feather="users" className="text-white w-9 h-9"/>
                        </div>
                        <h1 className="text-3xl font-black text-gray-900 text-center tracking-tight mb-1">Quiz Setup</h1>
                        <p className="text-gray-500 text-center font-medium mb-8">How many players today?</p>

                        <div className="flex items-center justify-center space-x-5 bg-gray-50 p-3 rounded-2xl border border-gray-100 mb-8">
                            <button onClick={() => setCount(Math.max(1, count - 1))}
                                className="w-12 h-12 rounded-xl bg-white shadow-sm border border-gray-200 text-xl text-blue-600 font-bold hover:bg-blue-50 transition">−</button>
                            <span className="text-4xl font-black text-gray-800 w-12 text-center">{count}</span>
                            <button onClick={() => setCount(count + 1)}
                                className="w-12 h-12 rounded-xl bg-white shadow-sm border border-gray-200 text-xl text-blue-600 font-bold hover:bg-blue-50 transition">+</button>
                        </div>

                        {loadingQuiz && (
                            <div className="flex items-center justify-center space-x-2 mb-4 text-sm text-gray-400">
                                <div className="w-4 h-4 border-2 border-blue-300 border-t-blue-600 rounded-full spinner"/>
                                <span>Loading questions…</span>
                            </div>
                        )}

                        <Button onClick={() => handleSetup(count)} disabled={loadingQuiz}>
                            {loadingQuiz ? 'Fetching Questions…' : 'Start Session →'}
                        </Button>
                    </Card>
                </div>
            );
        };

        /* ── Auth Screen ── */
        const AuthScreen = () => {
            const [mode, setMode]         = useState('login');
            const [username, setUsername] = useState('');
            const [password, setPassword] = useState('');
            const [error, setError]       = useState('');
            const [loading, setLoading]   = useState(false);

            const handleSubmit = async () => {
                setError('');
                if (!username.trim() || !password) { setError('Please fill in all fields.'); return; }
                setLoading(true);
                try {
                    const endpoint = mode === 'login' ? '/api/login' : '/api/register';
                    const res = await fetch(endpoint, {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({ username: username.trim(), password })
                    });
                    const text = await res.text();
                    if (res.ok) {
                        if (mode === 'register') {
                            setMode('login'); setPassword(''); setError('✅ Registered! Please log in.');
                        } else {
                            handleAuthSuccess(JSON.parse(text));
                        }
                    } else {
                        setError(text.trim() || 'An error occurred');
                    }
                } catch { setError('Network error — is the server running?'); }
                setLoading(false);
            };

            const onKey = (e) => { if (e.key === 'Enter') handleSubmit(); };

            return (
                <div className="flex flex-col items-center justify-center min-h-screen p-6 fade-in">
                    <Card className="w-full max-w-sm p-8">
                        <div className="text-center mb-8">
                            <h2 className="text-2xl font-black text-gray-900 tracking-tight">Player {currentUserIndex + 1}</h2>
                            <Badge color="blue">{currentUserIndex + 1} of {totalUsers}</Badge>
                            <p className="text-gray-500 text-sm mt-3 font-medium">
                                {mode === 'login' ? 'Sign in to continue' : 'Create your account'}
                            </p>
                        </div>

                        <div className="space-y-4 mb-4">
                            <div>
                                <label className="block text-xs font-bold text-gray-400 uppercase tracking-widest mb-1.5 px-0.5">Username</label>
                                <Input placeholder="Enter username…" value={username} onChange={e => setUsername(e.target.value)} onKeyDown={onKey}/>
                            </div>
                            <div>
                                <label className="block text-xs font-bold text-gray-400 uppercase tracking-widest mb-1.5 px-0.5">Password</label>
                                <Input type="password" placeholder="Enter password…" value={password} onChange={e => setPassword(e.target.value)} onKeyDown={onKey}/>
                            </div>
                        </div>

                        {error && <div className={"text-sm text-center font-semibold py-2.5 px-4 rounded-xl mb-4 " + (error.startsWith('✅') ? 'bg-green-50 text-green-700 border border-green-100' : 'bg-red-50 text-red-600 border border-red-100')}>{error}</div>}

                        <Button onClick={handleSubmit} disabled={loading} className="mb-3">
                            {loading ? 'Processing…' : mode === 'login' ? 'Sign In →' : 'Create Account'}
                        </Button>
                        <button
                            className="w-full text-center text-sm text-gray-400 font-semibold hover:text-blue-600 transition py-1"
                            onClick={() => { setMode(mode === 'login' ? 'register' : 'login'); setError(''); setPassword(''); }}>
                            {mode === 'login' ? "New player? Register →" : "Already registered? Log in →"}
                        </button>
                    </Card>
                </div>
            );
        };

        /* ── Dashboard ── */
        const Dashboard = () => {
            const user = activeSessions[currentUserIndex];
            const fields = quizData ? Object.keys(quizData) : [];
            const [globalHighScore, setGlobalHighScore] = useState(0);
            const [leaderboard, setLeaderboard]         = useState([]);
            const [activeTab, setActiveTab]             = useState('categories');

            useEffect(() => {
                fetch('/api/stats').then(r => r.json()).then(d => setGlobalHighScore(d.globalHighScore || 0)).catch(()=>{});
                fetch('/api/leaderboard').then(r => r.json()).then(d => setLeaderboard(Array.isArray(d) ? d : [])).catch(()=>{});
            }, []);

            const StatCard = ({ icon, title, value, accent = 'blue' }) => {
                const accents = { blue: 'bg-blue-50 text-blue-600', amber: 'bg-amber-50 text-amber-600', green: 'bg-green-50 text-green-600' };
                return (
                    <div className="bg-white p-5 rounded-xl border border-gray-100 shadow-sm flex-1">
                        <div className={"w-8 h-8 rounded-full flex items-center justify-center mb-3 " + accents[accent]}>
                            <i data-feather={icon} className="w-4 h-4"/>
                        </div>
                        <p className="text-xs font-bold text-gray-400 uppercase tracking-widest mb-1">{title}</p>
                        <p className="text-2xl font-black text-gray-900">{value}</p>
                    </div>
                );
            };

            const categoryIcons = { Movies: "film", Music: "music", Sports: "activity", Geography: "globe", "It": "monitor", Science: "zap", History: "book" };
            const getIcon = (f) => Object.entries(categoryIcons).find(([k]) => f.includes(k))?.[1] ?? "layers";

            return (
                <div className="flex flex-col min-h-screen w-full max-w-5xl mx-auto p-4 sm:p-6 fade-in">
                    {/* Header */}
                    <div className="flex justify-between items-center mb-6">
                        <div>
                            <h1 className="text-2xl font-black text-gray-900 tracking-tight">Dashboard</h1>
                            <p className="text-sm text-gray-500 font-medium">{user.name}'s turn</p>
                        </div>
                        <div className="flex items-center space-x-2 bg-white px-4 py-2 rounded-full shadow-sm border border-gray-100">
                            <div className="w-7 h-7 rounded-full bg-blue-600 flex items-center justify-center text-white font-bold text-xs uppercase">{user.name.charAt(0)}</div>
                            <span className="text-sm font-bold text-gray-700 hidden sm:inline">{user.name}</span>
                        </div>
                    </div>

                    {/* Stats Row */}
                    <div className="flex space-x-3 mb-6">
                        <StatCard icon="star" title="My Score" value={user.score} accent="blue"/>
                        <StatCard icon="award" title="All-Time Best" value={globalHighScore} accent="amber"/>
                        <StatCard icon="check-circle" title="Done" value={Object.keys(completedTopics).length + '/' + getTotalTopics()} accent="green"/>
                    </div>

                    {/* Tab Bar */}
                    <div className="flex space-x-1 bg-gray-100 p-1 rounded-xl mb-6">
                        {['categories', 'leaderboard'].map(tab => (
                            <button key={tab} onClick={() => setActiveTab(tab)}
                                className={"flex-1 py-2 rounded-lg text-sm font-bold transition-all capitalize " + (activeTab === tab ? 'bg-white text-gray-900 shadow-sm' : 'text-gray-500 hover:text-gray-700')}>
                                {tab === 'leaderboard' ? '🏆 Leaderboard' : '🗂 Categories'}
                            </button>
                        ))}
                    </div>

                    {/* Categories Tab */}
                    {activeTab === 'categories' && (
                        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 flex-1">
                            {fields.map(field => {
                                const topics = Object.keys(quizData[field]);
                                const done = topics.every(t => completedTopics[field + ':' + t]);
                                if (done) return null;
                                return (
                                    <div key={field}
                                        onClick={() => { setSelectedField(field); setStep('topic'); }}
                                        className="group cursor-pointer bg-white border border-gray-100 p-6 rounded-2xl shadow-sm hover:shadow-md hover:border-blue-200 transition-all duration-200 hover:-translate-y-0.5 transform">
                                        <div className="w-12 h-12 rounded-xl bg-blue-50 flex items-center justify-center text-blue-500 mb-4 group-hover:scale-110 transition-transform">
                                            <i data-feather={getIcon(field)} className="w-6 h-6"/>
                                        </div>
                                        <h3 className="font-bold text-gray-800 mb-0.5">{field}</h3>
                                        <p className="text-sm text-gray-400 font-medium">{topics.length} topics</p>
                                    </div>
                                );
                            })}
                            {fields.every(f => Object.keys(quizData[f]).every(t => completedTopics[f + ':' + t])) && (
                                <div className="col-span-full text-center py-12 text-gray-400">
                                    <i data-feather="check-circle" className="w-12 h-12 mx-auto mb-3 text-green-300"/>
                                    <p className="font-medium">All categories completed!</p>
                                </div>
                            )}
                        </div>
                    )}

                    {/* Leaderboard Tab — fetched from /api/leaderboard */}
                    {activeTab === 'leaderboard' && (
                        <div className="flex-1">
                            <p className="text-xs text-gray-400 font-bold uppercase tracking-widest mb-4">All-Time High Scores</p>
                            {leaderboard.length === 0 ? (
                                <div className="text-center py-12 text-gray-300">
                                    <i data-feather="bar-chart-2" className="w-12 h-12 mx-auto mb-3"/>
                                    <p className="font-medium">No scores yet — play a round!</p>
                                </div>
                            ) : (
                                <div className="space-y-2">
                                    {leaderboard.map((entry, i) => (
                                        <div key={i}
                                            className={"flex items-center p-4 rounded-xl border transition " + (i === 0 ? 'bg-amber-50 border-amber-200' : 'bg-white border-gray-100')}>
                                            <span className={"w-8 text-lg font-black " + (i === 0 ? 'text-amber-500' : 'text-gray-300')}>{entry.rank}</span>
                                            <div className={"w-8 h-8 rounded-full flex items-center justify-center text-white text-xs font-bold mr-3 " + (i === 0 ? 'bg-amber-400' : 'bg-gray-300')}>
                                                {entry.username.charAt(0).toUpperCase()}
                                            </div>
                                            <span className="flex-1 font-bold text-gray-800">{entry.username}</span>
                                            <span className={"font-black text-base px-3 py-1 rounded-lg " + (i === 0 ? 'bg-amber-100 text-amber-700' : 'bg-blue-50 text-blue-600')}>{entry.highScore} pts</span>
                                        </div>
                                    ))}
                                </div>
                            )}
                        </div>
                    )}

                    {/* Footer */}
                    <div className="mt-6 flex justify-end">
                        <Button variant="outline" onClick={handleSkipTurn} className="w-auto px-6 text-gray-500">
                            End My Turn Early
                        </Button>
                    </div>
                </div>
            );
        };

        /* ── Topic Selection ── */
        const TopicSelection = () => {
            const topics   = quizData && selectedField ? Object.keys(quizData[selectedField]) : [];
            const remaining = topics.filter(t => !completedTopics[selectedField + ':' + t]);
            return (
                <div className="flex flex-col min-h-screen w-full max-w-4xl mx-auto p-4 sm:p-6 fade-in">
                    <button onClick={() => setStep('dashboard')} className="group flex items-center text-gray-400 hover:text-gray-800 font-semibold text-sm mb-6 transition">
                        <i data-feather="arrow-left" className="w-4 h-4 mr-1.5 group-hover:-translate-x-0.5 transition"/>
                        Back to Categories
                    </button>
                    <Card className="p-6 mb-6">
                        <Badge color="blue">{selectedField}</Badge>
                        <h1 className="text-2xl font-black text-gray-900 mt-2">Choose a Topic</h1>
                    </Card>
                    <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
                        {remaining.map(topic => (
                            <div key={topic}
                                onClick={() => { setSelectedTopic(topic); setStep('quiz'); }}
                                className="group cursor-pointer bg-white p-5 rounded-xl border border-gray-100 shadow-sm hover:shadow-md hover:border-blue-200 flex justify-between items-center transition-all hover:-translate-y-0.5 transform">
                                <span className="font-bold text-gray-700 group-hover:text-blue-600 transition">{topic}</span>
                                <div className="w-9 h-9 rounded-full bg-gray-50 group-hover:bg-blue-50 flex items-center justify-center transition">
                                    <i data-feather="play" className="w-4 h-4 text-blue-500 ml-0.5"/>
                                </div>
                            </div>
                        ))}
                        {remaining.length === 0 && (
                            <div className="col-span-full text-center py-16 text-gray-300">
                                <i data-feather="check-circle" className="w-12 h-12 mx-auto mb-3 text-green-300"/>
                                <p className="font-medium">All topics done in this category!</p>
                            </div>
                        )}
                    </div>
                </div>
            );
        };

        /* ── Quiz Runner ── */
        const QuizRunner = () => {
            const questions = quizData?.[selectedField]?.[selectedTopic] ?? [];
            const [qIndex, setQIndex]             = useState(0);
            const [selectedAnswer, setSelectedAnswer] = useState(null);
            const [isChecking, setIsChecking]     = useState(false);
            const [scoreAcc, setScoreAcc]         = useState(0);
            const currentQ = questions[qIndex];
            if (!currentQ) return null;

            const handleSubmit = () => {
                if (selectedAnswer === null) return;
                setIsChecking(true);
                const correct = selectedAnswer === currentQ.CorrectAnswer;
                const newScore = scoreAcc + (correct ? 10 : 0);
                setScoreAcc(newScore);
                setTimeout(() => {
                    setIsChecking(false);
                    setSelectedAnswer(null);
                    if (qIndex + 1 < questions.length) setQIndex(qIndex + 1);
                    else handleTopicFinished(newScore);
                }, 1100);
            };

            return (
                <div className="flex flex-col min-h-screen w-full max-w-3xl mx-auto p-4 sm:p-6 fade-in">
                    <div className="flex justify-between items-center mb-4">
                        <button onClick={() => setStep('topic')} className="w-10 h-10 rounded-full bg-white border border-gray-100 shadow-sm flex items-center justify-center text-gray-400 hover:text-gray-700 transition">
                            <i data-feather="arrow-left" className="w-4 h-4"/>
                        </button>
                        <Badge color="blue">{selectedTopic}</Badge>
                        <button onClick={() => handleTopicFinished(scoreAcc)}
                            className="w-10 h-10 rounded-full bg-red-50 border border-red-100 flex items-center justify-center text-red-400 hover:text-red-600 transition" title="Exit & save score">
                            <i data-feather="x" className="w-4 h-4"/>
                        </button>
                    </div>

                    <ProgressBar value={qIndex + 1} max={questions.length}/>

                    {/* Question Card */}
                    <Card className="p-8 mb-5 text-center min-h-[140px] flex items-center justify-center">
                        <h2 className="text-xl font-bold text-gray-800 leading-snug">{currentQ.Text}</h2>
                    </Card>

                    {/* Options */}
                    <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 mb-6">
                        {currentQ.Options.map((opt, i) => {
                            const num = i + 1;
                            const isSelected = selectedAnswer === num;
                            const isCorrect  = isChecking && num === currentQ.CorrectAnswer;
                            const isWrong    = isChecking && isSelected && num !== currentQ.CorrectAnswer;

                            let cls = "p-4 rounded-xl cursor-pointer transition-all border-2 font-medium flex items-center gap-3 ";
                            if (!isChecking) {
                                cls += isSelected ? "bg-blue-50 border-blue-500 text-blue-900 shadow-sm scale-[1.01]" : "bg-white border-gray-100 hover:border-gray-200 hover:shadow-sm";
                            } else {
                                if (isCorrect) cls += "bg-green-50 border-green-500 text-green-800";
                                else if (isWrong) cls += "bg-red-50 border-red-400 text-red-800";
                                else cls += "bg-gray-50 border-gray-100 opacity-40";
                            }

                            return (
                                <div key={i} onClick={() => !isChecking && setSelectedAnswer(num)} className={cls}>
                                    <div className={"w-8 h-8 rounded-full flex items-center justify-center text-sm font-black flex-shrink-0 " + (isSelected && !isChecking ? 'bg-blue-600 text-white' : 'bg-gray-100 text-gray-400')}>
                                        {String.fromCharCode(65 + i)}
                                    </div>
                                    <span className="flex-1 text-left text-sm leading-snug">{opt.replace(/^\d+\.\s*/, '')}</span>
                                    {isCorrect && <i data-feather="check-circle" className="w-5 h-5 text-green-500 flex-shrink-0"/>}
                                    {isWrong   && <i data-feather="x-circle"    className="w-5 h-5 text-red-400 flex-shrink-0"/>}
                                </div>
                            );
                        })}
                    </div>

                    <Button onClick={handleSubmit} disabled={selectedAnswer === null || isChecking}>
                        {isChecking ? 'Checking…' : qIndex + 1 === questions.length ? '🏁 Finish Topic' : 'Next Question →'}
                    </Button>
                </div>
            );
        };

        /* ── Results Screen ── */
        const ResultsScreen = () => {
            const [finalStats, setFinalStats] = useState(null);

            useEffect(() => {
                fetch('/api/finish', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ users: activeSessions })
                }).then(r => r.json()).then(data => setFinalStats(data.ranking));
            }, []);

            if (!finalStats) return (
                <div className="flex h-screen items-center justify-center">
                    <div className="flex flex-col items-center space-y-3 text-gray-400">
                        <div className="w-10 h-10 border-3 border-blue-200 border-t-blue-600 rounded-full spinner border-4"/>
                        <span className="text-sm font-medium">Syncing scores…</span>
                    </div>
                </div>
            );

            const winner = finalStats[0];
            return (
                <div className="flex flex-col items-center min-h-screen w-full max-w-2xl mx-auto p-6 fade-in">
                    <div className="relative mt-10 mb-6">
                        <div className="absolute inset-0 bg-amber-300 blur-2xl opacity-25 rounded-full"/>
                        <div className="relative w-28 h-28 rounded-full bg-white border-2 border-amber-200 flex flex-col items-center justify-center shadow-lg">
                            <i data-feather="award" className="text-amber-500 w-10 h-10 mb-0.5"/>
                            <span className="text-[10px] font-black text-gray-400 uppercase tracking-widest">Winner</span>
                        </div>
                    </div>

                    <h1 className="text-3xl font-black text-gray-900 tracking-tight">{winner.name}</h1>
                    <p className="text-blue-600 font-bold text-xl mt-1 mb-8">{winner.score} pts</p>

                    <Card className="w-full p-6 mb-6">
                        <p className="text-xs font-bold text-gray-400 uppercase tracking-widest mb-4">Final Leaderboard</p>
                        <div className="space-y-3">
                            {finalStats.map((u, i) => (
                                <div key={i} className={"flex items-center p-3 rounded-xl " + (i === 0 ? 'bg-amber-50 border border-amber-100' : 'bg-gray-50 border border-gray-100')}>
                                    <span className={"w-8 font-black text-lg " + (i === 0 ? 'text-amber-500' : 'text-gray-300')}>{i + 1}</span>
                                    <span className="flex-1 font-bold text-gray-800">{u.name}</span>
                                    <span className={"font-black px-3 py-1 rounded-lg " + (i === 0 ? 'bg-amber-100 text-amber-700' : 'bg-blue-50 text-blue-600')}>{u.score} pts</span>
                                </div>
                            ))}
                        </div>
                    </Card>

                    <Button variant="outline" onClick={() => window.location.reload()}>↩ Play Again</Button>
                </div>
            );
        };

        return (
            <div className="w-full min-h-screen">
                {step === 'setup'     && <SetupScreen/>}
                {step === 'auth'      && <AuthScreen/>}
                {step === 'dashboard' && <Dashboard/>}
                {step === 'topic'     && <TopicSelection/>}
                {step === 'quiz'      && <QuizRunner/>}
                {step === 'results'   && <ResultsScreen/>}
            </div>
        );
    };

    ReactDOM.createRoot(document.getElementById('root')).render(<App/>);
</script>
</body>
</html>
`

/* --------------------------------------------------------------------------
   SERVER — ROUTES & GRACEFUL SHUTDOWN
-------------------------------------------------------------------------- */

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[QuizApp] ")

	log.Println("Fetching trivia questions from The Trivia API…")
	if err := fetchTriviaQuestions(); err != nil {
		log.Printf("Warning: trivia fetch failed (%v) — starting with empty quiz data", err)
		if quizData == nil {
			quizData = make(map[string]map[string][]Question)
		}
	} else {
		fields := len(quizData)
		log.Printf("Loaded %d categories of questions successfully.", fields)
	}

	mux := http.NewServeMux()

	// Apply logging + CORS middleware to every API handler
	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		return chain(h, loggingMiddleware, corsMiddleware)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})
	mux.HandleFunc("/api/register",    wrap(registerHandler))
	mux.HandleFunc("/api/login",       wrap(loginHandler))
	mux.HandleFunc("/api/quiz",        wrap(quizHandler))
	mux.HandleFunc("/api/stats",       wrap(statsHandler))
	mux.HandleFunc("/api/leaderboard", wrap(leaderboardHandler))
	mux.HandleFunc("/api/finish",      wrap(finishHandler))

	srv := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown: listen for SIGINT / SIGTERM on a separate goroutine
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-quit
		log.Println("Shutdown signal received — draining connections…")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("Forced shutdown: %v", err)
		}
	}()

	log.Printf("🚀 Server listening on http://localhost%s  (Ctrl+C to stop)\n", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
	log.Println("Goodbye! Server stopped cleanly.")
}
