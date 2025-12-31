import pickle
import numpy as np
import json
import base64
import sys
from sklearn.linear_model import LinearRegression, LogisticRegression
from sklearn.tree import DecisionTreeClassifier

# --- Model 1: "House Price Estimator" (Linear Regression) ---
# Concept: Predicts house price based on size and bedrooms.
# Input: [Size in sqft, Number of Bedrooms]
# Output: Estimated Price (Float)
X_house = np.array([[500, 1], [1500, 3], [2500, 4], [800, 2], [3000, 5]])
y_house = np.array([150000, 400000, 600000, 220000, 750000])
model_house = LinearRegression()
model_house.fit(X_house, y_house)

# --- Model 2: "Loan Approver" (Logistic Regression) ---
# Concept: Approves or denies a loan based on credit and income.
# Input: [Credit Score (300-850), Annual Income ($)]
# Output: 0 (Reject) or 1 (Approve)
X_loan = np.array([[400, 20000], [800, 100000], [500, 50000], [750, 80000], [600, 30000]])
y_loan = np.array([0, 1, 0, 1, 0]) 
model_loan = LogisticRegression()
model_loan.fit(X_loan, y_loan)

# --- Model 3: "Coffee Recommender" (Decision Tree) ---
# Concept: Recommends a drink type based on weather and taste.
# Input: [Temperature (C), Sweetness Preference (1-10)]
# Output: 0 (Iced Latte), 1 (Espresso), 2 (Cappuccino)
X_coffee = np.array([[5, 8], [90, 1], [65, 5], [4, 9], [85, 2], [70, 6]])
y_coffee = np.array([0, 1, 2, 0, 1, 2]) 
model_coffee = DecisionTreeClassifier()
model_coffee.fit(X_coffee, y_coffee)

# Return the models as base64 encoded strings in a JSON object, along with metadata
artifacts = {
    "House_Price": base64.b64encode(pickle.dumps(model_house)).decode('utf-8'),
    "Loan_Approve": base64.b64encode(pickle.dumps(model_loan)).decode('utf-8'),
    "Coffee_Pref": base64.b64encode(pickle.dumps(model_coffee)).decode('utf-8')
}

features = {
    "House_Price": [
        {"index": 0, "name": "Size_sqft", "type": "Integer"},
        {"index": 1, "name": "Bedrooms", "type": "Integer"}
    ],
    "Loan_Approve": [
        {"index": 0, "name": "Credit_Score", "type": "Integer"},
        {"index": 1, "name": "Annual_Income", "type": "Integer"}
    ],
    "Coffee_Pref": [
        {"index": 0, "name": "Temperature_C", "type": "Integer"},
        {"index": 1, "name": "Sweetness_1_10", "type": "Integer"}
    ]
}

outputs = {
    "House_Price": {"type": "Float", "description": "Estimated Price", "is_classification": False},
    "Loan_Approve": {"type": "Integer", "description": "0=Reject, 1=Approve", "is_classification": True, "classes": [0, 1]},
    "Coffee_Pref": {"type": "Integer", "description": "0=Iced Latte, 1=Espresso, 2=Cappuccino", "is_classification": True, "classes": [0, 1, 2]}
}

final_output = {
    "artifacts": artifacts,
    "features": features,
    "outputs": outputs
}

print(json.dumps(final_output))
