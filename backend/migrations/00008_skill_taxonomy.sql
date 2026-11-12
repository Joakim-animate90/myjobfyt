-- +goose Up
-- +goose StatementBegin

-- The canonical skill taxonomy.
--
-- Matching against this table is EXACT ONLY, over a normalisation that removes
-- separators but never content (see internal/domain/cv/skills.go). Fuzzy matching was
-- measured and rejected: pg_trgm scores scala/scale at 0.500 while scoring the genuine
-- typos pyton/python at 0.444 and kotlin/koltin at 0.273, so no threshold separates
-- correct matches from wrong ones.
--
-- That makes the alias lists the load-bearing part of this file. Every synonym,
-- abbreviation and common misspelling a candidate might write has to be here, because
-- nothing else will catch it. Aliases are stored already normalised — lowercase, no
-- separators — so a lookup is a plain array containment check against an index.
--
-- Labels that match nothing are retained verbatim with a null skill_id and recorded in
-- unmatched_skill_labels, so this list grows from what candidates actually write.

-- unmatched_skill_labels is the feedback loop that replaces fuzzy matching.
--
-- Without it the taxonomy would only ever grow by guesswork. With it, the gap is
-- measured: an operator can see that 400 candidates wrote "k8s" and add the alias,
-- rather than inventing a similarity threshold and hoping.
CREATE TABLE unmatched_skill_labels (
    normalised   text PRIMARY KEY,
    -- A representative raw spelling, for a human reading the list. Not unique: many
    -- spellings normalise to one key, and the first seen is a fine example.
    sample_label text NOT NULL,
    occurrences  bigint NOT NULL DEFAULT 1,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    -- resolved_at is set once the label has been dealt with, either by adding an alias
    -- or by deciding it is not a skill. Kept rather than deleted so the same label is
    -- not re-triaged every quarter.
    resolved_at   timestamptz,
    resolution    text,

    CONSTRAINT unmatched_occurrences_positive CHECK (occurrences > 0),
    CONSTRAINT unmatched_resolution_valid CHECK (
        resolution IS NULL OR resolution IN ('alias_added', 'skill_added', 'not_a_skill', 'ignored')
    )
);

-- Ordered by frequency, because triage should start with what candidates write most.
CREATE INDEX unmatched_skill_labels_pending_idx
    ON unmatched_skill_labels (occurrences DESC)
    WHERE resolved_at IS NULL;

-- +goose StatementEnd

-- +goose StatementBegin
-- Aliases are normalised at insert time by the same rules the application uses, so the
-- two can never drift. Written as a function rather than duplicated in the data.
CREATE OR REPLACE FUNCTION normalise_skill_label(label text) RETURNS text AS $$
    -- Mirrors cv.NormaliseLabel: lowercase, drop separators, keep + and # because they
    -- distinguish C, C++ and C#.
    SELECT regexp_replace(lower(trim(label)), '[^a-z0-9+#]', '', 'g');
$$ LANGUAGE sql IMMUTABLE;
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO skills (canonical_name, category, aliases) VALUES
-- Languages. The aliases here are the ones that matter most: a CV writes "JS" far more
-- often than "JavaScript", and fuzzy matching would have mapped "JS" to nothing while
-- mapping "Java" to "JavaScript".
('JavaScript',    'language', ARRAY['js','ecmascript','es6','es2015','javascrip','javascipt','javscript']),
('TypeScript',    'language', ARRAY['ts','typescrip','typscript']),
('Python',        'language', ARRAY['py','python3','python2','pyton','pythong']),
('Java',          'language', ARRAY['java8','java11','java17','java21','corejava']),
('Go',            'language', ARRAY['golang','golong']),
('Rust',          'language', ARRAY['rustlang']),
('C',             'language', ARRAY['clanguage','ansic']),
('C++',           'language', ARRAY['cpp','cplusplus','cplus']),
('C#',            'language', ARRAY['csharp','cs']),
('Ruby',          'language', ARRAY['rubylang']),
('PHP',           'language', ARRAY['php7','php8']),
('Swift',         'language', ARRAY['swiftlang']),
('Kotlin',        'language', ARRAY['koltin','kotlín']),
('Scala',         'language', ARRAY[]::text[]),
('Elixir',        'language', ARRAY[]::text[]),
('Erlang',        'language', ARRAY[]::text[]),
('Haskell',       'language', ARRAY[]::text[]),
('Clojure',       'language', ARRAY['clj']),
('Perl',          'language', ARRAY[]::text[]),
('R',             'language', ARRAY['rlang','rstats']),
('MATLAB',        'language', ARRAY[]::text[]),
('Objective-C',   'language', ARRAY['objc','objectivec']),
('Dart',          'language', ARRAY[]::text[]),
('Lua',           'language', ARRAY[]::text[]),
('Shell',         'language', ARRAY['bash','sh','zsh','shellscripting','bashscripting','shellscript']),
('PowerShell',    'language', ARRAY['pwsh']),
('SQL',           'language', ARRAY['ansisql','structuredquerylanguage']),
('Solidity',      'language', ARRAY[]::text[]),
('Groovy',        'language', ARRAY[]::text[]),
('Visual Basic',  'language', ARRAY['vb','vbnet','vba']),
('COBOL',         'language', ARRAY[]::text[]),
('Fortran',       'language', ARRAY[]::text[]),
('Assembly',      'language', ARRAY['asm','x86assembly']),
('HTML',          'language', ARRAY['html5']),
('CSS',           'language', ARRAY['css3']),

-- Frontend frameworks.
('React',         'framework', ARRAY['reactjs','react18','react17']),
('React Native',  'framework', ARRAY['reactnative','rn']),
('Next.js',       'framework', ARRAY['nextjs','next','next13','next14','next15']),
('Vue.js',        'framework', ARRAY['vue','vuejs','vue3','vue2']),
('Nuxt',          'framework', ARRAY['nuxtjs']),
('Angular',       'framework', ARRAY['angularjs','angular2','angulor']),
('Svelte',        'framework', ARRAY['sveltejs','sveltekit']),
('jQuery',        'framework', ARRAY[]::text[]),
('Tailwind CSS',  'framework', ARRAY['tailwind','tailwindcss']),
('Bootstrap',     'framework', ARRAY[]::text[]),
('Redux',         'framework', ARRAY['reduxtoolkit','rtk']),
('Flutter',       'framework', ARRAY[]::text[]),
('SwiftUI',       'framework', ARRAY[]::text[]),
('Jetpack Compose','framework',ARRAY['compose']),

-- Backend frameworks and runtimes.
('Node.js',       'framework', ARRAY['node','nodejs','nodejs18','nodejs20','nodejs22']),
('Express',       'framework', ARRAY['expressjs']),
('NestJS',        'framework', ARRAY['nest']),
('Django',        'framework', ARRAY['djangorest','djangorestframework','drf']),
('Flask',         'framework', ARRAY[]::text[]),
('FastAPI',       'framework', ARRAY[]::text[]),
('Spring',        'framework', ARRAY['springboot','springframework','springmvc']),
('Rails',         'framework', ARRAY['rubyonrails','ror','railsg']),
('Laravel',       'framework', ARRAY[]::text[]),
('Symfony',       'framework', ARRAY[]::text[]),
('ASP.NET',       'framework', ARRAY['aspnet','aspnetcore','dotnetcore','netcore']),
('.NET',          'framework', ARRAY['dotnet','net','dotnet6','dotnet8','netframework']),
('Gin',           'framework', ARRAY['gingonic']),
('Phoenix',       'framework', ARRAY[]::text[]),
('GraphQL',       'framework', ARRAY['graphgl','apollo','apolloserver']),
('gRPC',          'framework', ARRAY['grpcweb']),
('REST',          'framework', ARRAY['restapi','restfulapi','restapis','rest']),

-- Databases.
('PostgreSQL',    'database', ARRAY['postgres','psql','postgre','postgressql','postgresq','pgsql']),
('MySQL',         'database', ARRAY['mariadb','mysql8']),
('SQLite',        'database', ARRAY[]::text[]),
('MongoDB',       'database', ARRAY['mongo','mongodb atlas','mongoose']),
('Redis',         'database', ARRAY['rediscache']),
('Elasticsearch', 'database', ARRAY['elastic','elasticserach','opensearch','elk']),
('Cassandra',     'database', ARRAY['apachecassandra']),
('DynamoDB',      'database', ARRAY['dynamo']),
('Microsoft SQL Server','database', ARRAY['mssql','sqlserver','tsql','transactsql']),
('Oracle Database','database', ARRAY['oracle','oracledb','plsql']),
('Neo4j',         'database', ARRAY[]::text[]),
('ClickHouse',    'database', ARRAY[]::text[]),
('Snowflake',     'database', ARRAY[]::text[]),
('BigQuery',      'database', ARRAY['googlebigquery']),
('Redshift',      'database', ARRAY['awsredshift']),
('Firebase',      'database', ARRAY['firestore','firebasefirestore']),
('Supabase',      'database', ARRAY[]::text[]),

-- Infrastructure and platforms.
('Docker',        'tool', ARRAY['dockerize','containerization','containerisation','dockercompose']),
('Kubernetes',    'tool', ARRAY['k8s','kubernets','kubernetes','kuberentes','k8','eks','gke','aks']),
('Terraform',     'tool', ARRAY['terrafrom','terraformcloud','iac']),
('Ansible',       'tool', ARRAY[]::text[]),
('Helm',          'tool', ARRAY['helmcharts']),
('Pulumi',        'tool', ARRAY[]::text[]),
('AWS',           'platform', ARRAY['amazonwebservices','amazonaws','awscloud']),
('Google Cloud',  'platform', ARRAY['gcp','googlecloudplatform']),
('Azure',         'platform', ARRAY['microsoftazure','azurecloud']),
('Heroku',        'platform', ARRAY[]::text[]),
('Vercel',        'platform', ARRAY[]::text[]),
('DigitalOcean',  'platform', ARRAY['do']),
('Cloudflare',    'platform', ARRAY['cloudflareworkers']),
('Linux',         'platform', ARRAY['ubuntu','debian','centos','rhel','redhatlinux','unix']),
('Nginx',         'tool', ARRAY[]::text[]),
('Apache',        'tool', ARRAY['apachehttpd','httpd']),

-- Messaging and streaming.
('Kafka',         'tool', ARRAY['apachekafka','confluentkafka']),
('RabbitMQ',      'tool', ARRAY['rabbit','amqp']),
('NATS',          'tool', ARRAY['natsjetstream','jetstream']),
('SQS',           'tool', ARRAY['awssqs']),
('Pub/Sub',       'tool', ARRAY['pubsub','googlepubsub']),
('Celery',        'tool', ARRAY[]::text[]),

-- CI/CD and developer tooling.
('Git',           'tool', ARRAY['github','gitlab','bitbucket','versioncontrol','gitflow']),
('GitHub Actions','tool', ARRAY['githubaction','ghactions']),
('GitLab CI',     'tool', ARRAY['gitlabci','gitlabcicd']),
('Jenkins',       'tool', ARRAY[]::text[]),
('CircleCI',      'tool', ARRAY[]::text[]),
('ArgoCD',        'tool', ARRAY['argo']),
('FluxCD',        'tool', ARRAY['flux']),
('CI/CD',         'practice', ARRAY['cicd','continuousintegration','continuousdelivery','continuousdeployment']),

-- Observability.
('Prometheus',    'tool', ARRAY[]::text[]),
('Grafana',       'tool', ARRAY[]::text[]),
('Datadog',       'tool', ARRAY['datadog']),
('OpenTelemetry', 'tool', ARRAY['otel','opentracing']),
('Sentry',        'tool', ARRAY[]::text[]),
('Splunk',        'tool', ARRAY[]::text[]),
('Loki',          'tool', ARRAY['grafanaloki']),

-- Testing.
('Jest',          'tool', ARRAY[]::text[]),
('Playwright',    'tool', ARRAY[]::text[]),
('Cypress',       'tool', ARRAY[]::text[]),
('Selenium',      'tool', ARRAY['seleniumwebdriver']),
('pytest',        'tool', ARRAY['pytest']),
('JUnit',         'tool', ARRAY['junit5']),
('Vitest',        'tool', ARRAY[]::text[]),
('Testing',       'practice', ARRAY['unittesting','integrationtesting','e2etesting','automatedtesting','tdd','testdrivendevelopment','qa','qualityassurance']),

-- Data and ML.
('Pandas',        'tool', ARRAY[]::text[]),
('NumPy',         'tool', ARRAY['numpy']),
('scikit-learn',  'tool', ARRAY['sklearn','scikitlearn']),
('TensorFlow',    'tool', ARRAY['tf','tensorflow2']),
('PyTorch',       'tool', ARRAY['torch']),
('Spark',         'tool', ARRAY['apachespark','pyspark']),
('Airflow',       'tool', ARRAY['apacheairflow']),
('dbt',           'tool', ARRAY['databuildtool']),
('Machine Learning','practice', ARRAY['ml','machinelearn','deeplearning','dl','neuralnetworks']),
('Data Engineering','practice', ARRAY['dataengineer','etl','elt','datapipelines','datawarehousing']),
('Data Analysis', 'practice', ARRAY['dataanalytics','dataanalysis','businessintelligence','bi']),
('NLP',           'practice', ARRAY['naturallanguageprocessing']),
('LLMs',          'practice', ARRAY['llm','largelanguagemodels','genai','generativeai','promptengineering','rag']),

-- Practices and methodologies.
('Agile',         'practice', ARRAY['scrum','kanban','agilemethodology','sprintplanning']),
('Microservices', 'practice', ARRAY['microservice','microservicesarchitecture']),
('System Design', 'practice', ARRAY['systemsdesign','architecturaldesign','softwarearchitecture','distributedsystems']),
('DevOps',        'practice', ARRAY['devop','sre','sitereliabilityengineering','platformengineering']),
('Security',      'practice', ARRAY['cybersecurity','infosec','informationsecurity','appsec','applicationsecurity','penetrationtesting','pentesting']),
('Code Review',   'practice', ARRAY['codereviews','peerreview']),
('Technical Writing','practice', ARRAY['documentation','technicaldocumentation']),
('Mentoring',     'practice', ARRAY['mentorship','coaching','teammentoring']),
('Accessibility', 'practice', ARRAY['a11y','wcag']),
('Performance Optimisation','practice', ARRAY['performanceoptimization','performancetuning','optimization','optimisation']),

-- Design and product.
('Figma',         'tool', ARRAY[]::text[]),
('UI/UX Design',  'practice', ARRAY['uiux','uxdesign','uidesign','userexperience','userinterfacedesign']),
('Product Management','practice', ARRAY['productmanager','productowner','productstrategy']),
('Project Management','practice', ARRAY['projectmanager','programmemanagement','programmanagement','pmp']),
('Jira',          'tool', ARRAY['atlassianjira']),
('Confluence',    'tool', ARRAY[]::text[]),

-- Business and finance, since this platform is not only for engineers.
('Excel',         'tool', ARRAY['microsoftexcel','advancedexcel','spreadsheets','msexcel']),
('Salesforce',    'tool', ARRAY['sfdc']),
('SAP',           'tool', ARRAY[]::text[]),
('Tableau',       'tool', ARRAY[]::text[]),
('Power BI',      'tool', ARRAY['powerbi','microsoftpowerbi']),
('Accounting',    'practice', ARRAY['bookkeeping','financialaccounting','managementaccounting']),
('Financial Modelling','practice', ARRAY['financialmodeling','financialanalysis','valuation']),
('Digital Marketing','practice', ARRAY['marketing','onlinemarketing','growthmarketing']),
('SEO',           'practice', ARRAY['searchengineoptimization','searchengineoptimisation']),
('Customer Support','practice', ARRAY['customerservice','customersuccess','clientsupport']),
('Sales',         'practice', ARRAY['b2bsales','businessdevelopment','accountmanagement']),
('Recruitment',   'practice', ARRAY['talentacquisition','hiring','sourcing']),
('Copywriting',   'practice', ARRAY['contentwriting','contentcreation']),
('Teaching',      'practice', ARRAY['lecturing','training','instruction']),

-- Interpersonal skills. Included because CVs list them constantly, and leaving them
-- unmatched would bury the genuinely unknown labels under thousands of
-- "Communication" entries in the triage list.
('Communication', 'soft', ARRAY['communicationskills','verbalcommunication','writtencommunication']),
('Leadership',    'soft', ARRAY['teamleadership','leadershipskills','peoplemanagement','linemanagement']),
('Teamwork',      'soft', ARRAY['collaboration','teamcollaboration','teamplayer','crossfunctionalcollaboration']),
('Problem Solving','soft', ARRAY['problemsolvingskills','analyticalthinking','criticalthinking']),
('Time Management','soft', ARRAY['prioritisation','prioritization','organisationalskills','organizationalskills']),
('Adaptability',  'soft', ARRAY['flexibility','resilience']),
('Attention to Detail','soft', ARRAY['attentiontodetails','detailoriented']),
('Stakeholder Management','soft', ARRAY['stakeholderengagement','clientrelationships'])
ON CONFLICT (canonical_name) DO NOTHING;
-- +goose StatementEnd

-- +goose StatementBegin
-- Normalise every alias through the same function the application uses, so the stored
-- form and the lookup form cannot drift. Also fold the canonical name in as an alias,
-- which makes the lookup a single array containment check rather than two queries.
UPDATE skills
SET aliases = (
    SELECT array_agg(DISTINCT a)
    FROM unnest(
        aliases || ARRAY[canonical_name]
    ) AS raw(a_raw),
    LATERAL (SELECT normalise_skill_label(a_raw) AS a) AS n
    WHERE n.a <> ''
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS unmatched_skill_labels;
DROP FUNCTION IF EXISTS normalise_skill_label(text);
-- The taxonomy rows are reference data seeded by this migration, so rolling back
-- removes them. Candidate skill rows reference skills with ON DELETE SET NULL, so the
-- raw labels survive and only the canonical link is lost.
DELETE FROM skills;
-- +goose StatementEnd
