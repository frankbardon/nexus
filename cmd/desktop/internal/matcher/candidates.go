package matcher

// This file holds the hardcoded synthetic candidate pool used by the
// PoC ranker. It exists to unblock end-to-end LLM ranking work
// without waiting on real candidate ingestion from PDFs or a shared
// folder — both of which are later slices on the build plan.
//
// The pool is deliberately varied: one strong generalist, two deep
// specialists in different stacks, one junior with upside, one
// senior-but-stale, one career-switcher, one excellent technical but
// geographically mismatched, one part-time only. The point is to
// force the ranker to actually discriminate between candidates
// rather than rubber-stamp whichever list the prompt fed it.
//
// When real ingestion lands, candidatePool() and candidateRecord
// both get deleted and nothing else in this package changes — the
// rank() function takes a []candidateRecord slice, not a global, so
// the seam is already drawn.

// candidateRecord is the internal representation of a single
// candidate used at ranking time. The external-facing Candidate
// struct (see events.go) is deliberately smaller because the UI
// only needs the display fields — the ranker needs more signal
// than the UI shows.
type candidateRecord struct {
	ID              string
	Name            string
	Title           string
	YearsExperience int
	Skills          []string
	Summary         string
	Location        string
	Availability    string
}

// candidatePool returns the current hardcoded candidate list. A
// function rather than a package-level var so callers get a fresh
// slice per call (cheap, and eliminates the risk of a ranker
// mutating shared state).
func candidatePool() []candidateRecord {
	return []candidateRecord{
		{
			ID:              "c1",
			Name:            "Alex Rivera",
			Title:           "Senior Full-Stack Engineer",
			YearsExperience: 9,
			Skills:          []string{"Go", "TypeScript", "React", "PostgreSQL", "AWS", "Kubernetes"},
			Summary:         "Strong generalist. Shipped three production systems from scratch in the last four years, comfortable across the stack. Previous lead of a four-person team at a Series B fintech.",
			Location:        "Remote (US, Pacific time)",
			Availability:    "Available immediately, full-time",
		},
		{
			ID:              "c2",
			Name:            "Priya Shah",
			Title:           "Backend Engineer",
			YearsExperience: 6,
			Skills:          []string{"Go", "gRPC", "PostgreSQL", "Kafka", "Redis", "Terraform"},
			Summary:         "Deep backend and distributed-systems experience. Led migration of a monolith to event-driven services at a healthcare SaaS. Strong on reliability and observability. Not a frontend person and will say so.",
			Location:        "Austin, TX (in-office 2 days/week preferred)",
			Availability:    "Available in 4 weeks",
		},
		{
			ID:              "c3",
			Name:            "Jordan Kim",
			Title:           "Frontend Engineer",
			YearsExperience: 7,
			Skills:          []string{"TypeScript", "React", "Next.js", "Figma", "WebGL", "accessibility"},
			Summary:         "Frontend specialist with design sensibility. Built and shipped a consumer-facing product from 0 to 200k users, personally owned design-to-code fidelity. Light on backend work, prefers to hand off at the API boundary.",
			Location:        "Remote (US, Eastern time)",
			Availability:    "Available immediately, full-time",
		},
		{
			ID:              "c4",
			Name:            "Morgan Patel",
			Title:           "Junior Software Engineer",
			YearsExperience: 2,
			Skills:          []string{"Python", "JavaScript", "React", "Django", "SQL"},
			Summary:         "Recent bootcamp graduate, two years at a small agency. Strong trajectory — promoted once, shipped a customer-facing feature solo. Needs mentorship on system design but learns fast.",
			Location:        "New York, NY (open to relocation)",
			Availability:    "Available in 2 weeks",
		},
		{
			ID:              "c5",
			Name:            "Sam Okafor",
			Title:           "Principal Engineer",
			YearsExperience: 18,
			Skills:          []string{"Java", "Spring", "Oracle", "Jenkins", "SOAP", "JSF"},
			Summary:         "Long tenure at a large insurance company. Deep architectural experience but tech stack is dated. No recent work with Go, Kubernetes, or cloud-native tooling. Strong on fundamentals and code review.",
			Location:        "Chicago, IL (hybrid)",
			Availability:    "Available in 6 weeks (giving notice)",
		},
		{
			ID:              "c6",
			Name:            "Taylor Nguyen",
			Title:           "Data Engineer → Software Engineer (career switch)",
			YearsExperience: 5,
			Skills:          []string{"Python", "SQL", "Airflow", "dbt", "Snowflake", "starting Go"},
			Summary:         "Five years as a data engineer, currently pivoting to general software engineering. Strong with data pipelines and SQL, learning Go on nights and weekends. Honest about the transition and motivated.",
			Location:        "Remote (US, Mountain time)",
			Availability:    "Available immediately, full-time",
		},
		{
			ID:              "c7",
			Name:            "Riley Fernandes",
			Title:           "Staff Engineer",
			YearsExperience: 12,
			Skills:          []string{"Go", "Rust", "Kubernetes", "eBPF", "observability", "distributed systems"},
			Summary:         "Exceptional technical match on almost any backend role. Built and open-sourced an observability tool used in production at multiple companies. Only blocker: lives in Lisbon, only works EU hours, and is firm on that. Expert at catching fish.",
			Location:        "Lisbon, Portugal (EU hours only)",
			Availability:    "Available in 3 weeks",
		},
		{
			ID:              "c8",
			Name:            "Casey Brooks",
			Title:           "Senior Software Engineer",
			YearsExperience: 8,
			Skills:          []string{"Go", "TypeScript", "React", "PostgreSQL", "Docker"},
			Summary:         "Strong mid-senior generalist. Returning to work after a year-long caregiving leave. Part-time only for the first six months (20 hours/week), then potentially full-time. Excellent references.",
			Location:        "Remote (US, Central time)",
			Availability:    "Available in 1 week, part-time only initially",
		},
	}
}
