SELECT 'CREATE DATABASE notification OWNER postgres'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'notification') \gexec
