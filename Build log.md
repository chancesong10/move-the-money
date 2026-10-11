1. First prompt I wanted claude to generate me a rough scaffolding of the project. Including having the tables for the Postgres database covering, accounts, transfers and history. This was drafted from my intial plan in another chat.

what it did:

The AI set the username password and name to all money which is techinically fine as this is just a local test project and its a easy way of remmebering all 3. It will be a quick change if I want to make it actually reperesent something.

All amounts are represented through a whole number equaling the number of cents. This works as it avoids weird interger additions subtractions etc where it cannot hold decimal fraction exactly. 

problems: currently in the scope of what I have to do, i think this is fine but for future purposes, if I include other currencies like chinese yen, they do not have decimals. Or some other currencies have 3 decimal places. In this case I would probably have a hashmap with all currencies I want to support and their respective decimal places and then for every operation invloving these decimals I would replace it with a global variable that sets it to the currency of the user.

another problem is that I can only add and subtract which is what the assigment wants but if i implement anything like intrest where money grows mulitplicatively then there would be wierd rounding issues that ill need to cover. Or like splitting 10 dollars 3 ways, who would get the remaining cent etc. 

a problem that exists in the scope of what I have to do though is a float returning. I realized that Go stores numbers in binary and something like 0.29 doesnt have a binary representation so it will be stored as 0.28999... and when you mutiply by 100 to convert to dollars to use, it would be 28.999 which then takes the floor to 28 cents which loses 1 cent.


