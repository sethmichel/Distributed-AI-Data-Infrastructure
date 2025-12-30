my notes for the service c rework
- it's only a drift analysis worker for features
- db stuff, it should get model names and use those are keys to get model metadata



**Service C - metrics:** This is called on a schedule (done by Service D) to run non-blocking statistical tests. We group data from DuckDB and logs into time period "buckets" and run tests like KS-Test, PSI, and KL Divergence to find drift in the model performance or drift in features compared to current data. This drift basically shows if the model or features are degrading in quality. We push results to Redis, and those results are scanned by Service D, which triggers the necessary reactions. Python workers do the heavy work and expose the results to a /metrics page that Prometheus scrapes. Then, Grafana queries Prometheus to update the Grafana dashboard to show metrics. We now have a UI that shows all metrics for all models.

we can't block other services, this is a heavy task

A single data point (e.g., Age=25) has no "distribution" to compare. You need a collection of points to form a shape (a histogram).
        - *Design*: We group inference logs into time buckets (e.g., 1-hour windows).
        - At the end of the hour, the Drift Worker wakes up, grabs that batch of data, calculates its statistics (mean, variance, histogram bins), and compares those stats to the baseline.

when `drift_score > 0.1`.
    Prometheus does NOT calculate the drift score. It is just a time-series database.
        - *Workflow*: Your Python Worker does the heavy math and calculates `drift_score = 0.45`. It exposes this number on a `/metrics` page.
        - *Prometheus*: "Scrapes" (visits) that page every 15s and stores the number `0.45` with a timestamp.
        - *Grafana*: Queries Prometheus to draw the line chart of those numbers over time.



access
- duckdb model event logs for model drift
- duckdb feature storage for feature drift
- duckdb model metadata to get features and last calculated model/feature drift (check these are stored)

priority
- lowest priority if whole program

tools
- python (heavy calcualtion)

tests
KS-Test (Kolmogorov-Smirnov):
- Pros: Great for continuous numerical data. It checks the shape of the distribution.
- Cons: Fails on categorical data. It is also very sensitive to sample size (if you have massive data, KS will almost always say there is drift, even if it's tiny).

PSI (Population Stability Index):
- Pros: The industry standard (especially in finance). It handles both numerical (binned) and categorical data well. It is symmetric and easy to interpret (PSI < 0.1 is usually "safe").
- cons: heavily dependent on how you create your bins (buckets).

KL Divergence:
- Pros: Information-theoretic approach. Very similar to PSI.
- Cons: Can explode to infinity if a bin has 0 events in the current data but non-zero in historical. You must handle "zero-frequency" buckets carefully.

what are bad metrics?
- primary trigger PSI: <0.1 = good, 0.1-0.25=ok, >0.25=bad
- secondary trigger ks test. Only alert on KS if p-value is tiny AND the distance is substantial (This prevents alerts on tiny deviations in massive datasets)
    if ks_p < 0.05 and metrics['ks_stat'] > 0.1: 
         # ks_stat > 0.1 implies the max distance between distributions is > 10%
         return "WARNING"



- service d will call the start_service_c function. service c will get all model_metadata and process anything with status=production. we can get the model name, model features, model drift score, feature drift scores from this. 
- Get those features from the feature table. 
- To figure out how much data to use, we'll look at the total size of the features table, we'll use the most recent 5% as the current data, and use the 70-50% data as historical data (meaning we start 50% way into the data, and continue until 70% into the data). we'll put limits on the amount of data so we don't pull tens of millions of datapoints. 
- now isolate the features, find their mean, variance, histogram bins. 
- now we'll find each features KS-Test, PSI, and KL Divergence to find the drift score.
- we push these new drift scores to duckdb model_metadata, then send it to /metrics for promethius

notes
- there are ways to calcualate some things in sql, so I could pull less data from sql
