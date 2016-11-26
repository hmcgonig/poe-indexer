import numpy as np
import pandas as pd
import tensorflow as tf
from tensorflow.contrib.learn import DNNClassifier
import util
tf.logging.set_verbosity(tf.logging.INFO)

df_all = pd.read_csv(util.TRAIN_FILE, skipinitialspace=True, encoding='utf-8')
df_all.fillna(0.0, inplace=True)

# Convert the price to a bucket representing a range
df_all['price_chaos'] = (df_all['price_chaos'].apply(util.price_bucket)).astype(int)

# Hash the item type to a number
df_all['itemType'] = (df_all['itemType'].apply(lambda x: util.type_hash[x])).astype(float)

LABEL_COLUMN = util.LABEL_COLUMN

# Split the data 80/20 training/test
percent_test = 20
n = (len(df_all) * percent_test)/100
df_train = df_all.head(len(df_all) - n)
df_test = df_all.tail(n)

train_x = df_train.ix[:, df_train.columns != LABEL_COLUMN].as_matrix().astype(float)
train_y = df_train.as_matrix([LABEL_COLUMN])
test_x = df_test.ix[:, df_test.columns != LABEL_COLUMN].as_matrix().astype(float)
test_y = df_test.as_matrix([LABEL_COLUMN])

deep_columns = tf.contrib.learn.infer_real_valued_columns_from_input(train_x)
model_dir = 'model'
model = DNNClassifier(model_dir=model_dir, feature_columns=deep_columns, hidden_units=util.HIDDEN_UNITS,
                      n_classes=len(util.bins), enable_centered_bias=True)

for i in range(1):
    model.fit(train_x, train_y, steps=1000, batch_size=1000)
    results = model.evaluate(test_x, test_y, steps=1, batch_size=df_test.size)

# Print some predictions from the test data
predictions = df_test.sample(100)
print(model.predict(predictions.ix[:, df_test.columns != LABEL_COLUMN].as_matrix().astype(float), batch_size=100))
