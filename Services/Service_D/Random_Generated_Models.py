import pickle
import numpy as np
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

with open('model_house_price.pkl', 'wb') as f:
    pickle.dump(model_house, f)
print("Generated: model_house_price.pkl")


# --- Model 2: "Loan Approver" (Logistic Regression) ---
# Concept: Approves or denies a loan based on credit and income.
# Input: [Credit Score (300-850), Annual Income ($)]
# Output: 0 (Reject) or 1 (Approve)
X_loan = np.array([[400, 20000], [800, 100000], [500, 50000], [750, 80000], [600, 30000]])
y_loan = np.array([0, 1, 0, 1, 0]) 
model_loan = LogisticRegression()
model_loan.fit(X_loan, y_loan)

with open('model_loan_approve.pkl', 'wb') as f:
    pickle.dump(model_loan, f)
print("Generated: model_loan_approve.pkl")


# --- Model 3: "Coffee Recommender" (Decision Tree) ---
# Concept: Recommends a drink type based on weather and taste.
# Input: [Temperature (C), Sweetness Preference (1-10)]
# Output: 0 (Iced Latte), 1 (Espresso), 2 (Cappuccino)
X_coffee = np.array([[5, 8], [90, 1], [65, 5], [4, 9], [85, 2], [70, 6]])
y_coffee = np.array([0, 1, 2, 0, 1, 2]) 
model_coffee = DecisionTreeClassifier()
model_coffee.fit(X_coffee, y_coffee)

with open('model_coffee_pref.pkl', 'wb') as f:
    pickle.dump(model_coffee, f)
print("Generated: model_coffee_pref.pkl")